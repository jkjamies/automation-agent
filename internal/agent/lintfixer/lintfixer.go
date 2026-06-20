package lintfixer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/lintfixer/models"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/gitrepo"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// fixBranch is the single agent working branch per repo; retries push onto it so
// one reviewable PR accumulates the attempts.
const fixBranch = "automation-agent/lint-fix"

// GitHubClient is everything the orchestrator needs from githubapi (consumer-defined
// so it can be faked).
type GitHubClient interface {
	GitHub // FindAgentPRs, CreatePR, AddLabels (from applyfix.go)
	GetFileContent(ctx context.Context, owner, repo, path, ref string) (string, error)
	AttemptCount(ctx context.Context, owner, repo string, number int) (int, error)
}

// Deps configures the Fixer.
type Deps struct {
	LLM       model.LLM
	GH        GitHubClient
	Notify    notify.Notifier
	Token     string // for git clone/push over HTTPS
	Label     string
	CheckName string
	MaxIter   int
	Author    gitrepo.Author
	Log       *slog.Logger
	// CloneURL builds the clone URL for a repo; defaults to github.com HTTPS.
	// Overridable in tests.
	CloneURL func(owner, repo string) string
}

// Fixer orchestrates the autonomous lint-fix loop. The loop is event-driven across
// invocations (kickoff → suspend → CI webhook resume → loop or finish), not an
// in-process ADK loop, because CI takes 20–40 min. State lives on GitHub.
type Fixer struct {
	d Deps
}

// NewFixer builds a Fixer, applying sensible defaults.
func NewFixer(d Deps) *Fixer {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.MaxIter <= 0 {
		d.MaxIter = 3
	}
	if d.Label == "" {
		d.Label = "automation-agent"
	}
	if d.CheckName == "" {
		d.CheckName = "agent-lint-verify"
	}
	if d.Author.Name == "" {
		d.Author = gitrepo.Author{Name: "automation-agent", Email: "automation-agent@users.noreply.github.com"}
	}
	return &Fixer{d: d}
}

// Kickoff handles a KindLint envelope: triage → fetch → analyze → apply → suspend.
func (f *Fixer) Kickoff(ctx context.Context, raw []byte) error {
	k, err := models.ParseKickoff(raw)
	if err != nil {
		return err
	}
	f.d.Log.Info("lint-fix kickoff", "repo", k.Repo)
	return f.attempt(ctx, k.Owner(), k.Name(), k.Repo, k.Base, k.Base, k.ReportText(), "", true)
}

// ResumeInput is the normalized resume context (from a webhook event or a scan).
type ResumeInput struct {
	Owner, Repo, FullRepo string
	PRNumber              int
	Branch, HeadSHA       string
	Conclusion            string
	OutputText            string
}

// Resume handles a KindCI envelope (a GitHub check_run webhook).
func (f *Fixer) Resume(ctx context.Context, raw []byte) error {
	ev, err := githubapi.ParseCheckRunEvent(raw)
	if err != nil {
		return err
	}
	if ev.CheckName != f.d.CheckName || ev.Status != "completed" {
		return nil // not our check, or not finished yet
	}
	owner, name, _ := strings.Cut(ev.RepoFullName, "/")
	return f.HandleResume(ctx, ResumeInput{
		Owner: owner, Repo: name, FullRepo: ev.RepoFullName,
		PRNumber: ev.PRNumber, Branch: ev.PRBranch, HeadSHA: ev.HeadSHA,
		Conclusion: ev.Conclusion, OutputText: ev.OutputText,
	})
}

// HandleResume reacts to the agent verify check's conclusion: succeed, retry, or
// give up. It is also the entry point reconcile uses to recover missed webhooks.
func (f *Fixer) HandleResume(ctx context.Context, in ResumeInput) error {
	if in.PRNumber == 0 {
		return fmt.Errorf("resume: missing PR number")
	}
	link := pullURL(in.FullRepo, in.PRNumber)

	switch in.Conclusion {
	case "success":
		f.d.Log.Info("lint-fix succeeded", "repo", in.FullRepo, "pr", in.PRNumber)
		return f.notify(ctx, "Lint fix succeeded ✅", fmt.Sprintf("%s: the automated lint fix passed CI.", in.FullRepo), link)

	case "failure":
		attempts, err := f.d.GH.AttemptCount(ctx, in.Owner, in.Repo, in.PRNumber)
		if err != nil {
			return err
		}
		if attempts >= f.d.MaxIter {
			f.d.Log.Warn("lint-fix exhausted attempts", "repo", in.FullRepo, "pr", in.PRNumber, "attempts", attempts)
			return f.notify(ctx, "Lint fix needs human review ⚠️",
				fmt.Sprintf("%s: after %d attempts the lint fix still fails CI. Please review.", in.FullRepo, attempts), link)
		}
		f.d.Log.Info("lint-fix retrying", "repo", in.FullRepo, "pr", in.PRNumber, "next_attempt", attempts+1)
		feedback := "The previous attempt failed CI with:\n" + in.OutputText
		return f.attempt(ctx, in.Owner, in.Repo, in.FullRepo, "", fixBranch, in.OutputText, feedback, false)

	default:
		f.d.Log.Info("ignoring non-actionable check conclusion", "repo", in.FullRepo, "conclusion", in.Conclusion)
		return nil
	}
}

// attempt runs one fix: triage the report, read the current files, analyze fixes in
// parallel, and apply them as a commit + PR. readRef is the ref to read current file
// content from (base on kickoff, the agent branch on retry).
func (f *Fixer) attempt(ctx context.Context, owner, repo, fullRepo, base, readRef, report, feedback string, newBranch bool) error {
	files, err := Triage(ctx, f.d.LLM, report)
	if err != nil {
		return fmt.Errorf("%s: %w", fullRepo, err)
	}

	contents := make(map[string]string, len(files))
	for _, fp := range files {
		c, err := f.d.GH.GetFileContent(ctx, owner, repo, fp.Path, readRef)
		if err != nil {
			f.d.Log.Warn("skipping unreadable file", "repo", fullRepo, "path", fp.Path, "err", err)
			continue
		}
		contents[fp.Path] = c
	}

	edits, err := RunAnalyze(ctx, f.d.LLM, files, contents, feedback)
	if err != nil {
		return fmt.Errorf("%s: %w", fullRepo, err)
	}

	res, err := ApplyFix(ctx, f.d.GH, ApplyConfig{
		Owner: owner, Repo: repo, CloneURL: f.cloneURL(owner, repo), Token: f.d.Token,
		Base: base, Branch: fixBranch, NewBranch: newBranch, Label: f.d.Label,
		CommitMessage: "automation-agent: fix lint problems",
		PRTitle:       "automation-agent: fix lint problems",
		PRBody:        prBody(files),
		Author:        f.d.Author,
	}, edits)
	if err != nil {
		return fmt.Errorf("%s: %w", fullRepo, err)
	}
	f.d.Log.Info("lint-fix applied; awaiting CI", "repo", fullRepo, "pr", res.PR.Number, "sha", res.HeadSHA)
	return nil
}

func (f *Fixer) notify(ctx context.Context, title, text, link string) error {
	if f.d.Notify == nil {
		return nil
	}
	return f.d.Notify.Notify(ctx, notify.Message{Title: title, Text: text, Link: link})
}

func (f *Fixer) cloneURL(owner, repo string) string {
	if f.d.CloneURL != nil {
		return f.d.CloneURL(owner, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

func pullURL(fullRepo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", fullRepo, number)
}

func prBody(files []models.FileProblems) string {
	var b strings.Builder
	b.WriteString("Automated lint fix by automation-agent.\n\nFiles addressed:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s` (%d problem(s))\n", f.Path, len(f.Problems))
	}
	return b.String()
}
