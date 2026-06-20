package lintfixer

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// scriptedLLM routes responses: triage prompt → triage; analyze with CI feedback →
// fixRetry (a different fix, as a real feedback-driven retry would produce);
// otherwise → fix.
type scriptedLLM struct{ triage, fix, fixRetry string }

func (s scriptedLLM) Name() string { return "scripted" }
func (s scriptedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	resp := s.fix
	if req.Config != nil && strings.Contains(setup.ContentText(req.Config.SystemInstruction), "triaging") {
		resp = s.triage
	} else if s.fixRetry != "" && strings.Contains(setup.LastText(req.Contents), "previous attempt failed") {
		resp = s.fixRetry
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: setup.AssistantText(resp)}, nil)
	}
}

type fakeNotifier struct{ msgs []notify.Message }

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.msgs = append(n.msgs, m)
	return nil
}

func newFixer(remote string, gh *fakeGH, n *fakeNotifier) *Fixer {
	return NewFixer(Deps{
		LLM:       scriptedLLM{triage: `[{"path":"main.go","problems":["fmt imported and not used"]}]`, fix: "package main\n\nfunc main() {}\n", fixRetry: "package main\n\n// retry fix\nfunc main() {}\n"},
		GH:        gh,
		Notify:    n,
		Label:     "automation-agent",
		CheckName: "agent-lint-verify",
		MaxIter:   3,
		CloneURL:  func(_, _ string) string { return remote },
	})
}

const seedFile = "package main\n\nimport \"fmt\"\n\nfunc main() {}\n"

func TestKickoff(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{fileContents: map[string]string{"main.go": seedFile}}
	f := newFixer(remote, gh, &fakeNotifier{})

	raw := []byte(`{"repo":"acme/api","base":"master","report":"main.go:3 fmt imported and not used"}`)
	if err := f.Kickoff(context.Background(), raw); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.created == nil || gh.created.Head != fixBranch {
		t.Errorf("expected a PR on %s, got %+v", fixBranch, gh.created)
	}
	if len(gh.labeled) != 1 || gh.labeled[0] != "automation-agent" {
		t.Errorf("expected the agent label, got %v", gh.labeled)
	}
}

func TestResumeSuccess(t *testing.T) {
	n := &fakeNotifier{}
	f := newFixer(seedRemote(t), &fakeGH{}, n)

	err := f.HandleResume(context.Background(), ResumeInput{
		Owner: "acme", Repo: "api", FullRepo: "acme/api", PRNumber: 5, Conclusion: "success",
	})
	if err != nil {
		t.Fatalf("HandleResume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "succeeded") {
		t.Errorf("expected a success notification, got %+v", n.msgs)
	}
}

func TestResumeExhausted(t *testing.T) {
	n := &fakeNotifier{}
	gh := &fakeGH{attempts: 3}
	f := newFixer(seedRemote(t), gh, n)

	err := f.HandleResume(context.Background(), ResumeInput{
		Owner: "acme", Repo: "api", FullRepo: "acme/api", PRNumber: 5, Conclusion: "failure",
	})
	if err != nil {
		t.Fatalf("HandleResume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "human review") {
		t.Errorf("expected a needs-review notification, got %+v", n.msgs)
	}
}

func TestResumeRetry(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{fileContents: map[string]string{"main.go": seedFile}, attempts: 1}
	n := &fakeNotifier{}
	f := newFixer(remote, gh, n)

	// Kickoff first to create the agent branch on the remote.
	if err := f.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	// Now a failing CI with attempts(1) < max(3) should retry onto the same branch.
	gh.existing = []githubapi.PR{{Number: 5, Branch: fixBranch}}
	gh.created = nil // reset to detect any (unwanted) new PR
	err := f.HandleResume(context.Background(), ResumeInput{
		Owner: "acme", Repo: "api", FullRepo: "acme/api", PRNumber: 5, Conclusion: "failure",
		OutputText: "main.go still failing",
	})
	if err != nil {
		t.Fatalf("HandleResume retry: %v", err)
	}
	if gh.created != nil {
		t.Error("retry should reuse the existing PR, not create a new one")
	}
	if len(n.msgs) != 0 {
		t.Errorf("retry should not notify, got %+v", n.msgs)
	}
}

func TestResumeWebhookParsing(t *testing.T) {
	n := &fakeNotifier{}
	f := newFixer(seedRemote(t), &fakeGH{}, n)

	body := `{"action":"completed","check_run":{"name":"agent-lint-verify","status":"completed","conclusion":"success","head_sha":"s","pull_requests":[{"number":5,"head":{"ref":"automation-agent/lint-fix"}}]},"repository":{"full_name":"acme/api"}}`
	if err := f.Resume(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("expected a notification from the webhook resume, got %d", len(n.msgs))
	}
}

func TestResumeIgnoresOtherCheck(t *testing.T) {
	n := &fakeNotifier{}
	f := newFixer(seedRemote(t), &fakeGH{}, n)

	body := `{"check_run":{"name":"some-other-check","status":"completed","conclusion":"failure"},"repository":{"full_name":"acme/api"}}`
	if err := f.Resume(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 0 {
		t.Error("a non-agent check should be ignored")
	}
}
