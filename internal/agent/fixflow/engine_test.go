package fixflow

import (
	"context"
	"errors"
	"strings"
	"testing"

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
		GH: gh, Notify: n, MaxIter: 3, CloneURL: func(_, _ string) string { return remote },
	})
}

func TestEngineKickoff(t *testing.T) {
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
}

func TestEngineResumeSuccess(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	err := e.HandleResume(context.Background(), ResumeInput{
		Owner: "acme", Repo: "api", FullRepo: "acme/api", PRNumber: 5, Conclusion: "success",
	})
	if err != nil {
		t.Fatalf("HandleResume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "succeeded") {
		t.Errorf("expected success notification, got %+v", n.msgs)
	}
}

func TestEngineResumeExhausted(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{attempts: 3}, n)
	err := e.HandleResume(context.Background(), ResumeInput{
		Owner: "acme", Repo: "api", FullRepo: "acme/api", PRNumber: 5, Conclusion: "failure",
	})
	if err != nil {
		t.Fatalf("HandleResume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected needs-review notification, got %+v", n.msgs)
	}
}

func TestEngineResumeRetry(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{attempts: 1}
	n := &fakeNotifier{}
	e := newEngine(remote, gh, n)

	// Kickoff to create the branch on the remote.
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	// A different analyze result on retry so there is a real change to commit.
	e.spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		return []FileEdit{{Path: "a.go", Content: "package a\n\n// retry\n"}}, nil
	}
	gh.existing = []githubapi.PR{{Number: 5, Branch: "agent/fix"}}
	gh.created = nil
	err := e.HandleResume(context.Background(), ResumeInput{
		Owner: "acme", Repo: "api", FullRepo: "acme/api", PRNumber: 5, Conclusion: "failure", OutputText: "still failing",
	})
	if err != nil {
		t.Fatalf("HandleResume retry: %v", err)
	}
	if gh.created != nil {
		t.Error("retry should reuse the PR, not create a new one")
	}
	if len(n.msgs) != 0 {
		t.Errorf("retry should not notify, got %+v", n.msgs)
	}
}

func TestEngineResumeWebhook(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	body := `{"action":"completed","check_run":{"name":"agent-test-verify","status":"completed","conclusion":"success","pull_requests":[{"number":5,"head":{"ref":"agent/fix"}}]},"repository":{"full_name":"acme/api"}}`
	if err := e.Resume(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("expected a notification, got %d", len(n.msgs))
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
	e := NewEngine(spec, Deps{GH: &fakeGH{}, CloneURL: func(_, _ string) string { return seedRemote(t) }})
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","report":"r"}`)); err == nil {
		t.Fatal("expected triage error to propagate")
	}
}

func TestEngineLabelAndCheckName(t *testing.T) {
	e := newEngine("x", &fakeGH{}, &fakeNotifier{})
	if e.Label() != "automation-agent" || e.CheckName() != "agent-test-verify" {
		t.Errorf("label=%q check=%q", e.Label(), e.CheckName())
	}
}
