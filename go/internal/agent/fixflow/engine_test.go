package fixflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/githubapi"
)

// testSpec is a Spec with deterministic fake triage/analyze (no LLM), so the engine
// loop can be tested in isolation.
func testSpec() Spec {
	return Spec{
		Name: "test", Branch: "agent/fix", Label: "automation-agent", CheckName: "agent-test-verify",
		CommitMessage: "fix", PRTitle: "Fix",
		SuccessTitle: "Fix succeeded", ReviewTitle: "Needs human review",
		Triage: func(_ context.Context, _ model.LLM, _ string) ([]FileWork, error) {
			return []FileWork{{Path: "a.go", Items: []string{"x"}}}, nil
		},
		Analyze: func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
			return []FileEdit{{Path: "a.go", Content: "package a\n"}}, nil
		},
	}
}

func newEngine(remote string, gh *fakeGH, n *fakeNotifier) *Engine {
	return NewEngine(testSpec(), Deps{
		GH: gh, Notify: n, MaxIter: 3, CITimeout: time.Hour,
		CloneURL: func(_, _ string) string { return remote },
	})
}

// checkBody builds a check_run webhook payload for the test engine's check.
func checkBody(conclusion string, pr int, output string) []byte {
	return []byte(fmt.Sprintf(
		`{"action":"completed","check_run":{"name":"agent-test-verify","status":"completed","conclusion":%q,"pull_requests":[{"number":%d,"head":{"ref":"agent/fix"}}],"output":{"text":%q}},"repository":{"full_name":"acme/api"}}`,
		conclusion, pr, output))
}

// Kickoff applies a fix (creating the PR) and parks the run awaiting CI.
func TestEngineKickoffParks(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	e := newEngine(remote, gh, &fakeNotifier{})

	raw := []byte(`{"repo":"acme/api","base":"master","report":"r"}`)
	if err := e.Kickoff(context.Background(), raw); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.created == nil || gh.created.Head != "agent/fix" {
		t.Errorf("expected PR on agent/fix, got %+v", gh.created)
	}
	if len(gh.labeled) != 1 {
		t.Errorf("expected label, got %v", gh.labeled)
	}
	if e.driver.reg.Len() != 1 {
		t.Errorf("expected one parked run awaiting CI, got %d", e.driver.reg.Len())
	}
}

// A successful CI conclusion resolves the parked run and notifies success.
func TestEngineResumeSuccess(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "succeeded") {
		t.Errorf("expected success notification, got %+v", n.msgs)
	}
	if e.driver.reg.Len() != 0 {
		t.Errorf("success should free the parked run, got %d", e.driver.reg.Len())
	}
}

// A CI failure that has exhausted MaxIter asks for human review.
func TestEngineResumeExhausted(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	// Park a run that has already used all attempts.
	e.driver.reg.Park("acme/api#42", &ParkedRun{SessionID: "run-x", CallID: "c", Attempts: 3}, time.Hour, e.driver.onTimeout)

	if err := e.Resume(context.Background(), checkBody("failure", 42, "still broken")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected needs-review notification, got %+v", n.msgs)
	}
	if e.driver.reg.Len() != 0 {
		t.Errorf("exhausted run should be freed, got %d", e.driver.reg.Len())
	}
}

// A CI failure with attempts remaining re-applies on the same PR and re-parks.
func TestEngineResumeRetry(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	n := &fakeNotifier{}
	e := newEngine(remote, gh, n)

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	// A different analyze result on retry so there is a real change to commit.
	e.spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		return []FileEdit{{Path: "a.go", Content: "package a\n\n// retry\n"}}, nil
	}
	gh.existing = []githubapi.PR{{Number: 42, Branch: "agent/fix"}}
	gh.created = nil

	if err := e.Resume(context.Background(), checkBody("failure", 42, "still failing")); err != nil {
		t.Fatalf("Resume retry: %v", err)
	}
	if gh.created != nil {
		t.Error("retry should reuse the PR, not create a new one")
	}
	if len(n.msgs) != 0 {
		t.Errorf("retry should not notify, got %+v", n.msgs)
	}
	if e.driver.reg.Len() != 1 {
		t.Errorf("retry should leave the run parked, got %d", e.driver.reg.Len())
	}
}

// The full loop: kickoff → fail → fail → fail counts attempts in memory and gives up at
// MaxIter, proving tries are counted by the registry (not from GitHub).
func TestEngineFullLoopExhausts(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{existing: []githubapi.PR{{Number: 42, Branch: "agent/fix"}}}
	n := &fakeNotifier{}
	spec := testSpec()
	calls := 0
	spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		calls++ // vary content so every attempt is a real commit
		return []FileEdit{{Path: "a.go", Content: fmt.Sprintf("package a\n// v%d\n", calls)}}, nil
	}
	e := NewEngine(spec, Deps{GH: gh, Notify: n, MaxIter: 3, CITimeout: time.Hour,
		CloneURL: func(_, _ string) string { return remote }})

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	// Two failures are retried (attempts 2, 3); the third gives up.
	for i := 0; i < 2; i++ {
		if err := e.Resume(context.Background(), checkBody("failure", 42, "boom")); err != nil {
			t.Fatalf("Resume #%d: %v", i+1, err)
		}
		if len(n.msgs) != 0 {
			t.Fatalf("attempt %d should not notify yet, got %+v", i+2, n.msgs)
		}
		if e.driver.reg.Len() != 1 {
			t.Fatalf("attempt %d should re-park, got %d", i+2, e.driver.reg.Len())
		}
	}
	if err := e.Resume(context.Background(), checkBody("failure", 42, "boom")); err != nil {
		t.Fatalf("Resume final: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected needs-review after MaxIter, got %+v", n.msgs)
	}
	if e.driver.reg.Len() != 0 {
		t.Errorf("run should be freed after giving up, got %d", e.driver.reg.Len())
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 apply attempts, got %d", calls)
	}
}

// When CI never reports, the per-run timeout frees the run and asks for review.
func TestEngineTimeoutFreesRun(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	e.driver.reg.Park("acme/api#42", &ParkedRun{SessionID: "run-x", CallID: "c", Attempts: 1}, time.Hour, e.driver.onTimeout)

	e.driver.onTimeout("acme/api#42")
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected timeout review notification, got %+v", n.msgs)
	}
	if e.driver.reg.Len() != 0 {
		t.Errorf("timeout should free the run, got %d", e.driver.reg.Len())
	}
	// A late webhook after the timeout is a benign no-op.
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err != nil {
		t.Fatalf("late resume: %v", err)
	}
	if len(n.msgs) != 1 {
		t.Errorf("late webhook after timeout should not notify again, got %+v", n.msgs)
	}
}

// A conclusion for an unknown/already-resolved PR is a no-op.
func TestEngineResumeUnknownPR(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	if err := e.Resume(context.Background(), checkBody("success", 99, "")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 0 {
		t.Errorf("unknown PR should be ignored, got %+v", n.msgs)
	}
}

func TestEngineResumeIgnoresOtherCheck(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	body := `{"check_run":{"name":"some-other-check","status":"completed","conclusion":"failure"},"repository":{"full_name":"acme/api"}}`
	if err := e.Resume(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 0 {
		t.Error("a non-matching check should be ignored")
	}
}

func TestEngineKickoffTriageError(t *testing.T) {
	spec := testSpec()
	spec.Triage = func(context.Context, model.LLM, string) ([]FileWork, error) {
		return nil, errors.New("triage boom")
	}
	e := NewEngine(spec, Deps{GH: &fakeGH{}, CITimeout: time.Hour, CloneURL: func(_, _ string) string { return seedRemote(t) }})
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","report":"r"}`)); err == nil {
		t.Fatal("expected triage error to propagate")
	}
	if e.driver.reg.Len() != 0 {
		t.Errorf("a failed apply should not park a run, got %d", e.driver.reg.Len())
	}
}

func TestEngineLabelAndCheckName(t *testing.T) {
	e := newEngine("x", &fakeGH{}, &fakeNotifier{})
	if e.Label() != "automation-agent" || e.CheckName() != "agent-test-verify" {
		t.Errorf("label=%q check=%q", e.Label(), e.CheckName())
	}
}
