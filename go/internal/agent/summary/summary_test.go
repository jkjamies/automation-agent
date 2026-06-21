package summary

import (
	"context"
	"errors"
	"iter"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// --- fakes ---

type fakeLister struct {
	byRepo map[string][]githubapi.Commit
	err    error
}

func (f fakeLister) ListCommitsSince(_ context.Context, owner, repo string, _ time.Time) ([]githubapi.Commit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRepo[owner+"/"+repo], nil
}

type fakeNotifier struct{ msgs []notify.Message }

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.msgs = append(n.msgs, m)
	return nil
}

// stubLLM satisfies model.LLM without producing output (structure tests don't run
// the agent). It deliberately avoids importing genai to honor the ARCH rule.
type stubLLM struct{}

func (stubLLM) Name() string { return "stub" }
func (stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

type fakeState map[string]any

func (f fakeState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range f {
			if !yield(k, v) {
				return
			}
		}
	}
}

// --- tests ---

func TestFormatCommits(t *testing.T) {
	if got := formatCommits("o/r", nil); !strings.Contains(got, "no commits") {
		t.Errorf("empty = %q", got)
	}
	commits := []githubapi.Commit{{SHA: "abcdef1234", Message: "fix bug\n\ndetails", Author: "Jane"}}
	got := formatCommits("o/r", commits)
	if !strings.Contains(got, "abcdef1") || !strings.Contains(got, "fix bug") || !strings.Contains(got, "Jane") {
		t.Errorf("got = %q", got)
	}
	if strings.Contains(got, "details") {
		t.Errorf("should keep only the first line: %q", got)
	}
}

func TestBuildInstruction(t *testing.T) {
	st := fakeState{
		"commits:b/b": "repo B data",
		"commits:a/a": "repo A data",
		"other":       "ignore me",
	}
	got := buildInstruction("PROMPT", st)
	if !strings.HasPrefix(got, "PROMPT") {
		t.Errorf("prompt should lead: %q", got)
	}
	if strings.Contains(got, "ignore me") {
		t.Error("non-commits state key leaked into instruction")
	}
	if ai, bi := strings.Index(got, "repo A data"), strings.Index(got, "repo B data"); ai < 0 || bi < 0 || ai > bi {
		t.Errorf("commit sections not sorted: a=%d b=%d", ai, bi)
	}
}

func TestBuildInstructionEmpty(t *testing.T) {
	if got := buildInstruction("P", fakeState{}); !strings.Contains(got, "no commit data") {
		t.Errorf("got = %q", got)
	}
}

func TestSplitRepoAndSafeName(t *testing.T) {
	if _, _, ok := splitRepo("owner/repo"); !ok {
		t.Error("valid repo rejected")
	}
	if _, _, ok := splitRepo("bad"); ok {
		t.Error("invalid repo accepted")
	}
	if got := setup.SafeName("a/b:c"); got != "a_b_c" {
		t.Errorf("SafeName = %q, want a_b_c", got)
	}
}

func TestBuildSummaryAgentStructure(t *testing.T) {
	a, err := BuildSummaryAgent(Deps{
		LLM: stubLLM{}, GH: fakeLister{}, Notify: &fakeNotifier{}, Repos: []string{"o/r"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Name() != "summary_workflow" {
		t.Errorf("name = %q, want summary_workflow", a.Name())
	}
}

func TestBuildSummaryAgentValidation(t *testing.T) {
	if _, err := BuildSummaryAgent(Deps{LLM: stubLLM{}, GH: fakeLister{}, Notify: &fakeNotifier{}}); err == nil {
		t.Error("no repos should error")
	}
	if _, err := BuildSummaryAgent(Deps{Repos: []string{"o/r"}}); err == nil {
		t.Error("nil deps should error")
	}
}

// TestWorkflowRunWithStub drives the whole workflow through a real runner with a
// stub LLM (no live model), exercising the fetch and notify code agents and the
// runner plumbing deterministically.
func TestWorkflowRunWithStub(t *testing.T) {
	gh := fakeLister{byRepo: map[string][]githubapi.Commit{
		"o/r": {{SHA: "abc1234", Message: "do the thing", Author: "X"}},
	}}
	notifier := &fakeNotifier{}
	a, err := BuildSummaryAgent(Deps{LLM: stubLLM{}, GH: gh, Notify: notifier, Repos: []string{"o/r"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r, err := setup.NewRunner("stub-test", a)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := setup.Drive(context.Background(), r, "u", "s", "go"); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	// The stub produces no digest, so the notifier posts the fallback — but it must
	// still have been invoked exactly once.
	if len(notifier.msgs) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifier.msgs))
	}
}

func TestWorkflowFetchError(t *testing.T) {
	gh := fakeLister{err: errors.New("api down")}
	a, _ := BuildSummaryAgent(Deps{LLM: stubLLM{}, GH: gh, Notify: &fakeNotifier{}, Repos: []string{"o/r"}})
	r, _ := setup.NewRunner("stub-test", a)
	if err := setup.Drive(context.Background(), r, "u", "s", "go"); err == nil {
		t.Fatal("expected an error from a failing fetch")
	}
}

// TestLiveSummaryWorkflow runs the whole workflow end-to-end against a real Ollama
// server (opt-in via OLLAMA_LIVE), validating fetch -> state -> summarizer(LLM) ->
// OutputKey -> notify. Asserts only that a non-empty digest is posted.
func TestLiveSummaryWorkflow(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 to run the live summary workflow")
	}
	tag := os.Getenv("OLLAMA_MODEL")
	if tag == "" {
		tag = "gemma4:e4b"
	}
	llm, err := setup.NewOllamaModel("http://localhost:11434", tag)
	if err != nil {
		t.Fatal(err)
	}

	gh := fakeLister{byRepo: map[string][]githubapi.Commit{
		"acme/api": {{SHA: "1111111", Message: "add request rate limiting", Author: "Jane"}},
		"acme/web": {{SHA: "2222222", Message: "fix login redirect loop", Author: "Ravi"}},
	}}
	notifier := &fakeNotifier{}

	a, err := BuildSummaryAgent(Deps{LLM: llm, GH: gh, Notify: notifier, Repos: []string{"acme/api", "acme/web"}})
	if err != nil {
		t.Fatal(err)
	}

	r, err := setup.NewRunner("summary-test", a)
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Drive(context.Background(), r, "u", "s", "Run the daily digest."); err != nil {
		t.Fatalf("drive: %v", err)
	}

	if len(notifier.msgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifier.msgs))
	}
	if text := strings.TrimSpace(notifier.msgs[0].Text); text == "" || strings.Contains(text, "no digest") {
		t.Fatalf("empty or failed digest: %q", text)
	}
	t.Logf("digest:\n%s", notifier.msgs[0].Text)
}
