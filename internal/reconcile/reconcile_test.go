package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/notify"
)

type fakeGitHub struct {
	prs    map[string][]githubapi.PR        // repo -> PRs
	checks map[string]githubapi.CheckResult // headSHA -> check
	err    error
}

func (f *fakeGitHub) FindAgentPRs(_ context.Context, owner, repo, _ string) ([]githubapi.PR, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.prs[owner+"/"+repo], nil
}

func (f *fakeGitHub) AgentCheck(_ context.Context, _, _, ref, _ string) (githubapi.CheckResult, error) {
	return f.checks[ref], nil
}

type fakeNotifier struct{ msgs []notify.Message }

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.msgs = append(n.msgs, m)
	return nil
}

func cfg() Config {
	return Config{Repos: []string{"o/r"}, Label: "automation-agent", CheckName: "agent-lint-verify", CITimeout: 90 * time.Minute}
}

func TestScanResumeOnCompleted(t *testing.T) {
	gh := &fakeGitHub{
		prs:    map[string][]githubapi.PR{"o/r": {{Number: 5, HeadSHA: "sha5", URL: "u"}}},
		checks: map[string]githubapi.CheckResult{"sha5": {Found: true, Status: "completed", Conclusion: "failure"}},
	}
	var resumed []Action
	r := New(gh, &fakeNotifier{}, func(_ context.Context, a Action) error {
		resumed = append(resumed, a)
		return nil
	}, cfg())

	actions, err := r.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(actions) != 1 || actions[0].Outcome != OutcomeResume {
		t.Fatalf("actions = %+v", actions)
	}
	if len(resumed) != 1 || resumed[0].Check.Conclusion != "failure" {
		t.Errorf("resume not invoked with check: %+v", resumed)
	}
}

func TestScanTimeoutNotifies(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	gh := &fakeGitHub{
		prs: map[string][]githubapi.PR{"o/r": {{Number: 9, HeadSHA: "sha9", URL: "https://gh/pr/9"}}},
		checks: map[string]githubapi.CheckResult{
			"sha9": {Found: true, Status: "in_progress", StartedAt: now.Add(-2 * time.Hour)},
		},
	}
	notifier := &fakeNotifier{}
	r := New(gh, notifier, nil, cfg())
	r.now = func() time.Time { return now }

	actions, err := r.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if actions[0].Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %q, want timeout", actions[0].Outcome)
	}
	if len(notifier.msgs) != 1 || notifier.msgs[0].Link != "https://gh/pr/9" {
		t.Errorf("timeout notification = %+v", notifier.msgs)
	}
}

func TestScanPendingWithinBudget(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	gh := &fakeGitHub{
		prs: map[string][]githubapi.PR{"o/r": {{Number: 1, HeadSHA: "s1"}}},
		checks: map[string]githubapi.CheckResult{
			"s1": {Found: true, Status: "in_progress", StartedAt: now.Add(-5 * time.Minute)},
		},
	}
	r := New(gh, &fakeNotifier{}, nil, cfg())
	r.now = func() time.Time { return now }

	actions, _ := r.Scan(context.Background())
	if actions[0].Outcome != OutcomePending {
		t.Errorf("outcome = %q, want pending", actions[0].Outcome)
	}
}

func TestScanNoCheckYet(t *testing.T) {
	gh := &fakeGitHub{
		prs:    map[string][]githubapi.PR{"o/r": {{Number: 1, HeadSHA: "s1"}}},
		checks: map[string]githubapi.CheckResult{"s1": {Found: false}},
	}
	r := New(gh, &fakeNotifier{}, nil, cfg())
	actions, _ := r.Scan(context.Background())
	if actions[0].Outcome != OutcomeNoCheck {
		t.Errorf("outcome = %q, want nocheck", actions[0].Outcome)
	}
}

func TestScanInvalidRepoAndError(t *testing.T) {
	bad := New(&fakeGitHub{}, nil, nil, Config{Repos: []string{"no-slash"}})
	if _, err := bad.Scan(context.Background()); err == nil {
		t.Error("expected error for invalid repo spec")
	}

	failing := New(&fakeGitHub{err: errors.New("api down")}, nil, nil, cfg())
	if _, err := failing.Scan(context.Background()); err == nil {
		t.Error("expected error when FindAgentPRs fails")
	}
}
