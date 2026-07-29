package fixflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
	"automation-agent/internal/gitrepo"
	"automation-agent/internal/notify"
)

// FileWork is one file and the items to address in it (lint problems, uncovered
// regions, …) — the normalized output of a Spec's triage step.
type FileWork struct {
	Path  string
	Items []string
}

// ErrNoWork is what a Triage step returns when the report contains nothing actionable —
// the target is already clean. It is not a failure: the driver reports it as a positive
// "nothing to address" outcome (a clean ✅ notification) instead of asking a human to
// review a fix that was never needed. Triage steps wrap it with %w so errors.Is detects it.
var ErrNoWork = errors.New("no actionable work")

// TriageFunc normalizes an arbitrary tool report into per-file work (LLM-backed). It
// returns ErrNoWork (wrapped) when the report has nothing actionable.
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
	CheckName     string // e.g. agent-lint-verify
	CommitMessage string
	PRTitle       string
	SuccessTitle  string // notification title on success
	ReviewTitle   string // notification title when human review is needed
	CleanTitle    string // notification title when triage finds nothing to address
	Triage        TriageFunc
	Analyze       AnalyzeFunc
}

// Deps are the runtime dependencies shared by all engines. CodeLLM is the model for
// the code-change steps (typically larger); it falls back to LLM when nil. CITimeout
// bounds how long a single suspended run waits for its CI result before it is freed.
type Deps struct {
	LLM     model.LLM
	CodeLLM model.LLM
	GH      GitHub
	Notify  notify.Notifier
	// Provider authenticates git transport (https x-access-token) per op. A static
	// PAT in local dev, an auto-refreshed App installation token in production.
	Provider  gitrepo.TokenProvider
	MaxIter   int
	CITimeout time.Duration
	// OrphanTTL bounds how long a run that can never be resolved (created but never parked,
	// or displaced by a redelivered kickoff) lingers before the sweep reaps it. Defaults to
	// defaultOrphanTTL.
	OrphanTTL time.Duration
	// MaxFiles caps how many files one attempt edits (FIX_MAX_FILES). Non-positive disables
	// the cap. Files past it are dropped in triage's own order — which is its ranking — and
	// the number dropped is reported on the PR rather than silently swallowed.
	MaxFiles int
	// PRLabel is the single human-facing label applied to every agent PR on creation
	// (AGENT_PR_LABEL). Write-only — PR lookup is by branch, so it never gates behavior.
	PRLabel string
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
	// GitTransport selects the clone-URL scheme: "https" (default — token / GitHub App)
	// or "ssh" (local dev — ssh-agent/keys). It only changes the URL the default cloneURL
	// builds; a test-injected CloneURL overrides it.
	GitTransport string
	// SSHKey is the explicit private-key path used when GitTransport is "ssh" (GIT_SSH_KEY);
	// empty falls back to ssh-agent then default identities.
	SSHKey string
}

// Engine runs one Spec's event-driven fix loop. The CI-wait pause/resume itself is
// owned by the driver (a parking workflow graph + an injected setup.ParkStore backend).
type Engine struct {
	spec   Spec
	d      Deps
	driver *driver
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
	if d.OrphanTTL <= 0 {
		d.OrphanTTL = defaultOrphanTTL
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

// Name is the engine's workflow name ("lint" | "coverage"), used for logging.
func (e *Engine) Name() string { return e.spec.Name }

// defaultOrphanTTL is the fallback for Deps.OrphanTTL. Generous on purpose: an orphan costs
// storage, never correctness, so waiting a day to reap one is free, while reaping too eagerly
// would delete a run that is merely mid-apply.
const defaultOrphanTTL = 24 * time.Hour

// SweepTimeouts resolves this engine's parked runs whose CI never reported — the durable
// timeout catch-all driven by Cloud Scheduler via /internal/sweep.
func (e *Engine) SweepTimeouts(ctx context.Context) error { return e.driver.SweepTimeouts(ctx) }

// SweepOrphans deletes this engine's runs that nothing can ever resolve, freeing the park
// record (which holds the whole kickoff report) and the ADK session. Driven by Cloud
// Scheduler via /internal/sweep alongside SweepTimeouts.
func (e *Engine) SweepOrphans(ctx context.Context) error { return e.driver.SweepOrphans(ctx) }

// Kickoff handles a kickoff envelope: it starts a suspended fix run (apply → await CI).
func (e *Engine) Kickoff(ctx context.Context, raw []byte) error {
	k, err := parseKickoff(raw)
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

// resumeInput is the normalized resume context derived from a check_run webhook. The
// parked run already holds the owner/repo/branch from kickoff, so resume only needs the
// PR identity, the conclusion, and the CI output (used as retry feedback).
type resumeInput struct {
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
	return e.driver.Resume(ctx, resumeInput{
		FullRepo:   ev.RepoFullName,
		PRNumber:   ev.PRNumber,
		Conclusion: ev.Conclusion,
		OutputText: ev.OutputText,
	})
}

// attemptOnce runs a single fix attempt against rp: triage → checkout → analyze →
// commit, returning the resulting PR. It is the body the apply_fix tool invokes; the
// surrounding suspend/retry loop lives in the driver. One checkout is shared by analyze
// (read/explore) and commit (write/push).
func (e *Engine) attemptOnce(ctx context.Context, rp *runParams) (applyResult, error) {
	work, err := e.spec.Triage(ctx, e.d.LLM, rp.report)
	if err != nil {
		return applyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}
	work, dropped := e.capWork(work)
	if dropped > 0 {
		e.d.Log.Warn("capping the files this attempt will edit",
			"workflow", e.spec.Name, "repo", rp.fullRepo, "editing", len(work), "dropped", dropped)
	}

	cfg := applyConfig{
		Owner: rp.owner, Repo: rp.repo, CloneURL: e.cloneURL(rp.owner, rp.repo), Provider: e.d.Provider, SSHKey: e.d.SSHKey,
		Base: rp.base, Branch: e.spec.Branch, Label: e.d.PRLabel,
		CommitMessage: e.spec.CommitMessage, PRTitle: e.spec.PRTitle, PRBody: prBody(e.spec, work, dropped),
		Author: e.d.Author,
	}

	gitRepo, err := openCheckout(ctx, cfg)
	if err != nil {
		return applyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}
	defer func() { _ = os.RemoveAll(gitRepo.Dir()) }()

	edits, err := e.spec.Analyze(ctx, AnalyzeInput{LLM: e.d.LLM, CodeLLM: e.d.CodeLLM, RepoDir: gitRepo.Dir(), Work: work, Feedback: rp.feedback, Log: e.d.Log})
	if err != nil {
		return applyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
	}

	res, err := commitEdits(ctx, e.d.GH, gitRepo, cfg, edits)
	if err != nil {
		return applyResult{}, fmt.Errorf("%s %s: %w", rp.fullRepo, e.spec.Name, err)
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
	if e.d.GitTransport == "ssh" {
		return fmt.Sprintf("git@github.com:%s/%s.git", owner, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

func pullURL(fullRepo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", fullRepo, number)
}

// capWork trims the attempt to MaxFiles, returning the kept work and how many files were
// dropped. Triage's ordering is its ranking, so the kept files are the ones it put first.
func (e *Engine) capWork(work []FileWork) (kept []FileWork, dropped int) {
	if e.d.MaxFiles <= 0 || len(work) <= e.d.MaxFiles {
		return work, 0
	}
	return work[:e.d.MaxFiles], len(work) - e.d.MaxFiles
}

func prBody(spec Spec, work []FileWork, dropped int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Automated %s fix by automation-agent.\n\nFiles addressed:\n", spec.Name)
	for _, f := range work {
		fmt.Fprintf(&b, "- `%s` (%d item(s))\n", f.Path, len(f.Items))
	}
	// Say what was left out. A cap that silently drops work reads as "this is everything",
	// and the reader has no way to tell the difference.
	if dropped > 0 {
		fmt.Fprintf(&b, "\n%d further file(s) were reported but not addressed in this PR "+
			"(capped at %d per attempt). Re-run once these land to pick up the rest.\n", dropped, len(work))
	}
	return b.String()
}
