// Package reconcile implements the stateless recovery scan for the lint-fixer:
// GitHub is the source of truth, so on startup and on a timer we list labeled PRs,
// read each one's agent verify check, and resume finished ones or time out stuck
// ones. No local database. See docs/architecture.md §8. Deterministic tooling.
package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// GitHub is the narrow slice of githubapi the scan needs (consumer-defined so it
// can be faked in tests).
type GitHub interface {
	FindAgentPRs(ctx context.Context, owner, repo, label string) ([]githubapi.PR, error)
	AgentCheck(ctx context.Context, owner, repo, ref, checkName string) (githubapi.CheckResult, error)
}

// Outcome classifies what the scan decided about a PR.
type Outcome string

const (
	OutcomePending Outcome = "pending" // check still running, within budget
	OutcomeNoCheck Outcome = "nocheck" // check hasn't appeared yet
	OutcomeResume  Outcome = "resume"  // check completed -> hand to lint-fixer
	OutcomeTimeout Outcome = "timeout" // pending past CITimeout -> human review
)

// Action is the scan's decision for one PR.
type Action struct {
	Repo    string
	PR      githubapi.PR
	Check   githubapi.CheckResult
	Outcome Outcome
}

// ResumeFunc is invoked when a PR's check has completed. The lint-fixer wires this
// in a later phase; it decides pass/fail and loops or finishes.
type ResumeFunc func(ctx context.Context, a Action) error

// Config parameterizes the scan.
type Config struct {
	Repos     []string // owner/repo entries
	Label     string
	CheckName string
	CITimeout time.Duration
}

// Reconciler scans labeled PRs across repos.
type Reconciler struct {
	gh       GitHub
	notifier notify.Notifier
	resume   ResumeFunc
	cfg      Config
	now      func() time.Time
}

// New builds a Reconciler. resume may be nil until the lint-fixer is wired.
func New(gh GitHub, notifier notify.Notifier, resume ResumeFunc, cfg Config) *Reconciler {
	return &Reconciler{gh: gh, notifier: notifier, resume: resume, cfg: cfg, now: time.Now}
}

// Scan runs one pass over all repos, taking the side effect for each PR (resume or
// timeout notification) and returning every decision for observability. Per-repo
// errors are collected but do not abort the whole scan.
func (r *Reconciler) Scan(ctx context.Context) ([]Action, error) {
	var actions []Action
	var errs []error

	for _, repo := range r.cfg.Repos {
		owner, name, ok := splitRepo(repo)
		if !ok {
			errs = append(errs, fmt.Errorf("invalid repo %q (want owner/repo)", repo))
			continue
		}
		prs, err := r.gh.FindAgentPRs(ctx, owner, name, r.cfg.Label)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, pr := range prs {
			act, err := r.handlePR(ctx, repo, owner, name, pr)
			if err != nil {
				errs = append(errs, err)
			}
			actions = append(actions, act)
		}
	}
	return actions, joinErrs(errs)
}

func (r *Reconciler) handlePR(ctx context.Context, repo, owner, name string, pr githubapi.PR) (Action, error) {
	check, err := r.gh.AgentCheck(ctx, owner, name, pr.HeadSHA, r.cfg.CheckName)
	if err != nil {
		return Action{Repo: repo, PR: pr, Outcome: OutcomePending}, err
	}

	act := Action{Repo: repo, PR: pr, Check: check, Outcome: r.classify(check)}
	switch act.Outcome {
	case OutcomeResume:
		if r.resume != nil {
			return act, r.resume(ctx, act)
		}
	case OutcomeTimeout:
		return act, r.notifyTimeout(ctx, act)
	}
	return act, nil
}

func (r *Reconciler) classify(c githubapi.CheckResult) Outcome {
	if !c.Found {
		return OutcomeNoCheck
	}
	if c.Status == "completed" {
		return OutcomeResume
	}
	if !c.StartedAt.IsZero() && r.now().Sub(c.StartedAt) > r.cfg.CITimeout {
		return OutcomeTimeout
	}
	return OutcomePending
}

func (r *Reconciler) notifyTimeout(ctx context.Context, a Action) error {
	if r.notifier == nil {
		return nil
	}
	return r.notifier.Notify(ctx, notify.Message{
		Title: "Lint-fixer needs human review",
		Text:  fmt.Sprintf("%s PR #%d: CI check %q has been pending past the timeout.", a.Repo, a.PR.Number, r.cfg.CheckName),
		Link:  a.PR.URL,
	})
}

func splitRepo(s string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("reconcile: %s", strings.Join(msgs, "; "))
}
