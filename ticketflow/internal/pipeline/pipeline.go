// Package pipeline is the orchestration layer: one Jira ticket in, one pull request (or a
// clear reason why not) out, with no human in between and none of the merge authority.
//
// The run is an ADK graph workflow. Deterministic steps are function nodes; judgement
// calls are LLM agent nodes; the coding itself is delegated to a harness.Harness. Every
// way a run can end is a named edge into one of three terminal nodes:
//
//	START → intake      ─ready→     prepare      ─ok→      planner → triage_plan
//	        intake      ─not_ready→ bounce
//	        prepare     ─blocked→   escalate
//	        triage_plan ─go→        implement → verify
//	        triage_plan ─not_ready→ bounce
//	        triage_plan ─invalid→   escalate
//	        verify      ─pass→      reviewer → triage_review
//	        verify      ─retry→     implement           (bounded by MaxAttempts)
//	        verify      ─give_up→   escalate
//	        triage_review ─revise→  implement           (bounded by MaxRevisions)
//	        triage_review ─publish→ publish
//
// publish opens a pull request; bounce returns an unready ticket with questions; escalate
// hands a claimed ticket back to humans with the reason. Nothing merges.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	"ticketflow/internal/gate"
	"ticketflow/internal/github"
	"ticketflow/internal/harness"
	"ticketflow/internal/jira"
	"ticketflow/internal/workspace"
)

// Tracker is the issue-tracker side of a run (Jira).
type Tracker interface {
	Issue(ctx context.Context, key string) (jira.Issue, error)
	Comment(ctx context.Context, key, text string) error
	EditLabels(ctx context.Context, key string, add, remove []string) error
	Transition(ctx context.Context, key, name string) error
}

// CodeHost is the code-hosting side of a run (GitHub). It has no merge operation.
type CodeHost interface {
	DefaultBranch(ctx context.Context, repo string) (string, error)
	OpenPR(ctx context.Context, repo string, pr github.NewPR) (github.PR, error)
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
}

// Settings are the run policies.
type Settings struct {
	TriggerLabel     string
	ClaimLabel       string
	NeedsHumanLabel  string
	ReviewTransition string // empty skips the Jira transition after the PR opens
	ProjectRepos     map[string]string
	RepoAllowed      func(repo string) bool
	BaseBranch       string // empty means the repository default
	BranchPrefix     string
	PRLabel          string
	DraftPRs         bool
	Author           workspace.Author
	MaxAttempts      int
	MaxRevisions     int
	Limits           gate.DiffLimits
	Readiness        gate.ReadinessRules
	CheckTimeout     time.Duration
	WorkRoot         string
	CloneURL         func(repo string) string
	GitToken         string
}

// Deps are a pipeline's collaborators.
type Deps struct {
	Tracker  Tracker
	Host     CodeHost
	Harness  harness.Harness
	Planner  model.LLM
	Reviewer model.LLM
	Settings Settings
	Log      *slog.Logger
}

// Status is how a run ended.
type Status string

// The ways a run ends.
const (
	StatusPublished Status = "published" // a pull request is open for human review
	StatusDryRun    Status = "dry_run"   // a pull request would have been opened
	StatusBounced   Status = "bounced"   // the ticket was not ready; questions posted
	StatusEscalated Status = "escalated" // the agent gave up; a human was asked to take over
)

// Outcome describes a finished run.
type Outcome struct {
	Status  Status
	Key     string
	Repo    string
	Branch  string
	PRURL   string
	Reason  string
	Details []string
	Patch   string // the proposed change, for dry runs
	PRBody  string // the pull request description, for dry runs
}

// Pipeline runs tickets.
type Pipeline struct {
	d   Deps
	log *slog.Logger
}

// New validates deps and applies defaults.
func New(d Deps) (*Pipeline, error) {
	var missing []string
	if d.Tracker == nil {
		missing = append(missing, "Tracker")
	}
	if d.Host == nil {
		missing = append(missing, "Host")
	}
	if d.Harness == nil {
		missing = append(missing, "Harness")
	}
	if d.Planner == nil {
		missing = append(missing, "Planner")
	}
	if d.Reviewer == nil {
		missing = append(missing, "Reviewer")
	}
	if d.Settings.CloneURL == nil {
		missing = append(missing, "Settings.CloneURL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("pipeline: missing %s", strings.Join(missing, ", "))
	}
	s := &d.Settings
	if s.RepoAllowed == nil {
		s.RepoAllowed = func(string) bool { return false }
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = 3
	}
	if s.CheckTimeout <= 0 {
		s.CheckTimeout = 20 * time.Minute
	}
	if s.BranchPrefix == "" {
		s.BranchPrefix = "ticketflow/"
	}
	if s.Readiness.MinDescription == 0 && s.Readiness.AllowedTypes == nil && s.Readiness.BlockLabels == nil {
		s.Readiness = gate.DefaultReadinessRules
	}
	if s.Author.Name == "" {
		s.Author = workspace.Author{Name: "ticketflow", Email: "ticketflow@users.noreply.github.com"}
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{d: d, log: log}, nil
}

// RunOptions tune a single run.
type RunOptions struct {
	// DryRun does everything up to publishing — including the coding, checks, and
	// self-review — but pushes nothing, opens no PR, and writes nothing to Jira.
	DryRun bool
	// KeepWorkdir leaves the checkout on disk for inspection.
	KeepWorkdir bool
}

// Run takes one ticket as far as it can go. Every run that gets past reading the ticket
// ends on a terminal node that reports back to Jira; if the graph itself fails, Run
// escalates on its behalf so a claimed ticket is never left silent.
func (p *Pipeline) Run(ctx context.Context, key string, opts RunOptions) (Outcome, error) {
	if err := os.MkdirAll(p.d.Settings.WorkRoot, 0o755); err != nil && p.d.Settings.WorkRoot != "" {
		return Outcome{}, fmt.Errorf("pipeline: work root: %w", err)
	}
	dir, err := os.MkdirTemp(p.d.Settings.WorkRoot, "ticketflow-"+safeName(key)+"-")
	if err != nil {
		return Outcome{}, fmt.Errorf("pipeline: workdir: %w", err)
	}
	if !opts.KeepWorkdir {
		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				p.log.Warn("remove workdir", "dir", dir, "err", err)
			}
		}()
	}
	r := &run{p: p, key: key, dir: dir, dryRun: opts.DryRun, budget: p.d.Settings.MaxAttempts,
		log: p.log.With("ticket", key, "dry_run", opts.DryRun)}
	wf, err := p.graph(r)
	if err != nil {
		return Outcome{}, err
	}
	r.log.InfoContext(ctx, "run started", "workdir", dir)
	_, runErr := harness.RunAgent(ctx, wf, key)
	if r.outcome == nil {
		reason := "the run ended without reaching a terminal step"
		if runErr != nil {
			reason = runErr.Error()
		}
		if r.issue.Key == "" {
			// The ticket was never read, so nothing was claimed and there is nobody to tell.
			return Outcome{Key: key, Status: StatusEscalated, Reason: reason}, errors.Join(errors.New("pipeline: run failed before intake"), runErr)
		}
		r.escalateNow(ctx, reason, nil)
	}
	r.log.InfoContext(ctx, "run finished", "status", r.outcome.Status, "pr", r.outcome.PRURL, "reason", r.outcome.Reason)
	return *r.outcome, runErr
}

// Workflow builds the graph for a run against key without executing it, for inspection
// (e.g. the ADK dev UI) and tests.
func (p *Pipeline) Workflow(key, dir string) (agent.Agent, error) {
	return p.graph(&run{p: p, key: key, dir: dir, budget: p.d.Settings.MaxAttempts, log: p.log})
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, s)
}
