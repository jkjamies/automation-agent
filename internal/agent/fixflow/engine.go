package fixflow

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/adk/model"

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

// GitHubClient is everything the engine needs from githubapi.
type GitHubClient interface {
	GitHub // FindAgentPRs, CreatePR, AddLabels
	AttemptCount(ctx context.Context, owner, repo string, number int) (int, error)
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
}

// Coder returns the code-change model, falling back to the default model when no
// dedicated code model is set.
func (in AnalyzeInput) Coder() model.LLM {
	if in.CodeLLM != nil {
		return in.CodeLLM
	}
	return in.LLM
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
// the code-change steps (typically larger); it falls back to LLM when nil.
type Deps struct {
	LLM      model.LLM
	CodeLLM  model.LLM
	GH       GitHubClient
	Notify   notify.Notifier
	Token    string
	MaxIter  int
	Author   gitrepo.Author
	Log      *slog.Logger
	CloneURL func(owner, repo string) string // overridable in tests
}

// Engine runs one Spec's event-driven fix loop.
type Engine struct {
	spec Spec
	d    Deps
}

// NewEngine builds an engine, applying defaults.
func NewEngine(spec Spec, d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.MaxIter <= 0 {
		d.MaxIter = 3
	}
	if d.Author.Name == "" {
		d.Author = gitrepo.Author{Name: "automation-agent", Email: "automation-agent@users.noreply.github.com"}
	}
	if d.CodeLLM == nil {
		d.CodeLLM = d.LLM
	}
	return &Engine{spec: spec, d: d}
}

// CheckName is the agent verify check this engine resumes on.
func (e *Engine) CheckName() string { return e.spec.CheckName }

// Kickoff handles a kickoff envelope: triage → analyze → apply → suspend.
func (e *Engine) Kickoff(ctx context.Context, raw []byte) error {
	k, err := ParseKickoff(raw)
	if err != nil {
		return err
	}
	e.d.Log.Info("fix kickoff", "workflow", e.spec.Name, "repo", k.Repo)
	return e.attempt(ctx, k.Owner(), k.Name(), k.Repo, k.Base, k.ReportText(), "", true)
}

// ResumeInput is the normalized resume context (from a webhook event or a scan).
type ResumeInput struct {
	Owner, Repo, FullRepo string
	PRNumber              int
	Branch, HeadSHA       string
	Conclusion            string
	OutputText            string
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
	owner, name, _ := strings.Cut(ev.RepoFullName, "/")
	return e.HandleResume(ctx, ResumeInput{
		Owner: owner, Repo: name, FullRepo: ev.RepoFullName,
		PRNumber: ev.PRNumber, Branch: ev.PRBranch, HeadSHA: ev.HeadSHA,
		Conclusion: ev.Conclusion, OutputText: ev.OutputText,
	})
}

// HandleResume reacts to the check conclusion: succeed, retry, or give up. Also the
// entry point reconcile uses to recover missed webhooks.
func (e *Engine) HandleResume(ctx context.Context, in ResumeInput) error {
	if in.PRNumber == 0 {
		return fmt.Errorf("resume: missing PR number")
	}
	link := pullURL(in.FullRepo, in.PRNumber)

	switch in.Conclusion {
	case "success":
		e.d.Log.Info("fix succeeded", "workflow", e.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber)
		return e.notify(ctx, e.spec.SuccessTitle, fmt.Sprintf("%s: %s passed CI.", in.FullRepo, e.spec.Name), link)

	case "failure":
		attempts, err := e.d.GH.AttemptCount(ctx, in.Owner, in.Repo, in.PRNumber)
		if err != nil {
			return err
		}
		if attempts >= e.d.MaxIter {
			e.d.Log.Warn("fix exhausted attempts", "workflow", e.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "attempts", attempts)
			return e.notify(ctx, e.spec.ReviewTitle,
				fmt.Sprintf("%s: after %d attempts the %s fix still fails CI. Please review.", in.FullRepo, attempts, e.spec.Name), link)
		}
		e.d.Log.Info("fix retrying", "workflow", e.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "next_attempt", attempts+1)
		feedback := "The previous attempt failed CI with:\n" + in.OutputText
		return e.attempt(ctx, in.Owner, in.Repo, in.FullRepo, "", in.OutputText, feedback, false)

	default:
		e.d.Log.Info("ignoring non-actionable conclusion", "workflow", e.spec.Name, "repo", in.FullRepo, "conclusion", in.Conclusion)
		return nil
	}
}

func (e *Engine) attempt(ctx context.Context, owner, repo, fullRepo, base, report, feedback string, newBranch bool) error {
	work, err := e.spec.Triage(ctx, e.d.LLM, report)
	if err != nil {
		return fmt.Errorf("%s %s: %w", fullRepo, e.spec.Name, err)
	}

	cfg := ApplyConfig{
		Owner: owner, Repo: repo, CloneURL: e.cloneURL(owner, repo), Token: e.d.Token,
		Base: base, Branch: e.spec.Branch, NewBranch: newBranch, Label: e.spec.Label,
		CommitMessage: e.spec.CommitMessage, PRTitle: e.spec.PRTitle, PRBody: prBody(e.spec, work),
		Author: e.d.Author,
	}

	// One checkout, shared by analyze (read/explore) and commit (write/push).
	gitRepo, err := Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("%s %s: %w", fullRepo, e.spec.Name, err)
	}
	defer os.RemoveAll(gitRepo.Dir())

	edits, err := e.spec.Analyze(ctx, AnalyzeInput{LLM: e.d.LLM, CodeLLM: e.d.CodeLLM, RepoDir: gitRepo.Dir(), Work: work, Feedback: feedback})
	if err != nil {
		return fmt.Errorf("%s %s: %w", fullRepo, e.spec.Name, err)
	}

	res, err := Commit(ctx, e.d.GH, gitRepo, cfg, edits)
	if err != nil {
		return fmt.Errorf("%s %s: %w", fullRepo, e.spec.Name, err)
	}
	e.d.Log.Info("fix applied; awaiting CI", "workflow", e.spec.Name, "repo", fullRepo, "pr", res.PR.Number, "sha", res.HeadSHA)
	return nil
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
