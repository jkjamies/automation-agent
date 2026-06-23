package fixflow

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/gitrepo"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// FileWork is one file and the items to address in it (lint problems, uncovered
// regions, …) — the normalized output of a Spec's triage step.
type FileWork struct {
	Path  string
	Items []string
}

// TriageFunc normalizes an arbitrary tool report into per-file work (LLM-backed).
type TriageFunc func(ctx context.Context, llm model.LLM, report string) ([]FileWork, error)

// AnalyzeInput is what an AnalyzeFunc receives. RepoDir is the checked-out working
// tree: analyze reads source from it (and may explore it), and the engine commits
// whatever edits are returned from the same checkout. LLM is the default model
// (planning/exploration); CodeLLM is the (often larger) model for writing code.
type AnalyzeInput struct {
	LLM      model.LLM
	CodeLLM  model.LLM
	RepoDir  string
	Work     []FileWork
	Feedback string // previous attempt's CI failure, on retry
	Log      *slog.Logger
}

// Coder returns the code-change model, falling back to the default model when no
// dedicated code model is set.
func (in AnalyzeInput) Coder() model.LLM {
	if in.CodeLLM != nil {
		return in.CodeLLM
	}
	return in.LLM
}

// Logger returns the injected logger, or the default logger when none was set
// (e.g. in tests that construct an AnalyzeInput directly).
func (in AnalyzeInput) Logger() *slog.Logger {
	if in.Log != nil {
		return in.Log
	}
	return slog.Default()
}

// AnalyzeFunc produces the whole-file edits to apply (rewritten source, new tests, …).
type AnalyzeFunc func(ctx context.Context, in AnalyzeInput) ([]FileEdit, error)

// Spec is the per-workflow configuration that turns the engine into a concrete
// fixing agent (lint, coverage, …).
type Spec struct {
	Name          string // "lint" | "coverage"
	Branch        string // e.g. automation-agent/lint-fix
	Label         string // e.g. automation-agent
	CheckName     string // e.g. agent-lint-verify
	CommitMessage string
	PRTitle       string
	SuccessTitle  string // notification title on success
	ReviewTitle   string // notification title when human review is needed
	Triage        TriageFunc
	Analyze       AnalyzeFunc
}

// Deps are the runtime dependencies shared by all engines. CodeLLM is the model for
// the code-change steps (typically larger); it falls back to LLM when nil. CITimeout
// bounds how long a single suspended run waits for its CI result before it is freed.
type Deps struct {
	LLM       model.LLM
	CodeLLM   model.LLM
	GH        GitHub
	Notify    notify.Notifier
	Token     string
	MaxIter   int
	CITimeout time.Duration
	// Repos is the kickoff allowlist (REPOS). When non-empty, a kickoff whose repo is not
	// listed is rejected; empty imposes no restriction (restriction is opt-in).
	Repos  []string
	Author gitrepo.Author
	Log    *slog.Logger
	// SessionService stores the durable suspend/resume history for the parked fix loop.
	// Nil falls back to in-memory (a restart strands parked runs); a durable backend
	// (sqlite/firestore) lets a parked run resume after a restart. Built once at startup.
	SessionService session.Service
	// ParkStore persists the park record (prKey→session, attempts, run params) so a resume
	// — and, with a durable backend, a restart — can reconstruct it. Nil falls back to the
	// in-memory store. Built once at startup, alongside SessionService.
	ParkStore setup.ParkStore
	CloneURL  func(owner, repo string) string // overridable in tests
}

// Engine runs one Spec's event-driven fix loop. The CI-wait suspend/resume itself is
// owned by the Driver (ADK IsLongRunning + an injected setup.ParkStore backend).
type Engine struct {
	spec   Spec
	d      Deps
	driver *Driver
}

// NewEngine builds an engine, applying defaults. It panics if the long-run agent cannot
// be constructed — that only happens on a programming error (a malformed tool schema),
// not at runtime.
func NewEngine(spec Spec, d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.MaxIter <= 0 {
		d.MaxIter = 3
	}
	if d.CITimeout <= 0 {
		d.CITimeout = 90 * time.Minute
	}
	if d.Author.Name == "" {
		d.Author = gitrepo.Author{Name: "automation-agent", Email: "automation-agent@users.noreply.github.com"}
	}
	if d.CodeLLM == nil {
		d.CodeLLM = d.LLM
	}
	e := &Engine{spec: spec, d: d}
	driver, err := newDriver(e)
	if err != nil {
		panic(fmt.Sprintf("fixflow: build %s driver: %v", spec.Name, err))
	}
	e.driver = driver
	return e
}

// CheckName is the agent verify check this engine resumes on.
func (e *Engine) CheckName() string { return e.spec.CheckName }

// SweepTimeouts resolves this engine's parked runs whose CI never reported — the durable
// timeout catch-all driven by Cloud Scheduler via /internal/sweep.
func (e *Engine) SweepTimeouts(ctx context.Context) error { return e.driver.SweepTimeouts(ctx) }

// Kickoff handles a kickoff envelope: it starts a suspended fix run (apply → await CI).
func (e *Engine) Kickoff(ctx context.Context, raw []byte) error {
	k, err := ParseKickoff(raw)
	if err != nil {
		return err
	}
	if !e.repoAllowed(k.Repo) {
		e.d.Log.Warn("fix kickoff rejected: repo not in allowlist", "workflow", e.spec.Name, "repo", k.Repo)
		return fmt.Errorf("kickoff: repo %q not in the configured allowlist", k.Repo)
	}
	e.d.Log.Info("fix kickoff", "workflow", e.spec.Name, "repo", k.Repo)
	return e.driver.Kickoff(ctx, k)
}

// repoAllowed reports whether repo may be targeted by a kickoff. An empty allowlist
// (REPOS unset) imposes no restriction; otherwise the repo must be listed.
func (e *Engine) repoAllowed(repo string) bool {
	return len(e.d.Repos) == 0 || slices.Contains(e.d.Repos, repo)
}

// ResumeInput is the normalized resume context derived from a check_run webhook. The
// parked run already holds the owner/repo/branch from kickoff, so resume only needs the
// PR identity, the conclusion, and the CI output (used as retry feedback).
type ResumeInput struct {
	FullRepo   string
	PRNumber   int
	Conclusion string
	OutputText string
}

// Resume handles a GitHub check_run webhook. It no-ops unless the event is this
// engine's check completing — so multiple engines can each be handed the event.
func (e *Engine) Resume(ctx context.Context, raw []byte) error {
	ev, err := githubapi.ParseCheckRunEvent(raw)
	if err != nil {
		return err
	}
	if ev.CheckName != e.spec.CheckName || ev.Status != "completed" {
		return nil
	}
	return e.driver.Resume(ctx, ResumeInput{
		FullRepo:   ev.RepoFullName,
		PRNumber:   ev.PRNumber,
		Conclusion: ev.Conclusion,
		OutputText: ev.OutputText,
	})
}

// attemptOnce runs a single fix attempt against rp: triage → checkout → analyze →
// commit, returning the resulting PR. It is the body the apply_fix tool invokes; the
// surrounding suspend/retry loop lives in the Driver. One checkout is shared by analyze
// (read/explore) and commit (write/push).
func (e *Engine) attemptOnce(ctx context.Context, rp *runParams) (ApplyResult, error) {
	work, err := e.spec.Triage(ctx, e.d.LLM, rp.report)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}

	cfg := ApplyConfig{
		Owner: rp.owner, Repo: rp.repo, CloneURL: e.cloneURL(rp.owner, rp.repo), Token: e.d.Token,
		Base: rp.base, Branch: e.spec.Branch, NewBranch: rp.newBranch, Label: e.spec.Label,
		CommitMessage: e.spec.CommitMessage, PRTitle: e.spec.PRTitle, PRBody: prBody(e.spec, work),
		Author: e.d.Author,
	}

	gitRepo, err := Open(ctx, cfg)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}
	defer os.RemoveAll(gitRepo.Dir())

	edits, err := e.spec.Analyze(ctx, AnalyzeInput{LLM: e.d.LLM, CodeLLM: e.d.CodeLLM, RepoDir: gitRepo.Dir(), Work: work, Feedback: rp.feedback, Log: e.d.Log})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}

	res, err := Commit(ctx, e.d.GH, gitRepo, cfg, edits)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}
	return res, nil
}

func (e *Engine) notify(ctx context.Context, title, text, link string) error {
	if e.d.Notify == nil {
		return nil
	}
	return e.d.Notify.Notify(ctx, notify.Message{Title: title, Text: text, Link: link})
}

func (e *Engine) cloneURL(owner, repo string) string {
	if e.d.CloneURL != nil {
		return e.d.CloneURL(owner, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

func pullURL(fullRepo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", fullRepo, number)
}

func prBody(spec Spec, work []FileWork) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Automated %s fix by automation-agent.\n\nFiles addressed:\n", spec.Name)
	for _, f := range work {
		fmt.Fprintf(&b, "- `%s` (%d item(s))\n", f.Path, len(f.Items))
	}
	return b.String()
}
