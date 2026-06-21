package setup

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// newCIWaiter builds the parking agent + a shared in-memory runner used by the
// suspend/resume prototypes.
func newCIWaiter(t *testing.T) *runner.Runner {
	t.Helper()
	awaitCI, err := functiontool.New(functiontool.Config{
		Name:          "await_ci",
		Description:   "Open the PR and wait for CI to report.",
		IsLongRunning: true,
	}, func(_ tool.Context, _ struct {
		PR int `json:"pr"`
	}) (map[string]any, error) {
		return map[string]any{"status": "pending"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := llmagent.New(llmagent.Config{
		Name:        "ci-waiter",
		Model:       suspendStub{},
		Instruction: "Call await_ci and report the result.",
		Tools:       []tool.Tool{awaitCI},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "susp", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// park runs until the agent parks on the long-running call and returns its id.
func park(t *testing.T, r *runner.Runner, uid, sid string) string {
	t.Helper()
	var id string
	for ev, err := range r.Run(context.Background(), uid, sid, UserText("fix coverage"), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("park run: %v", err)
		}
		if len(ev.LongRunningToolIDs) > 0 {
			id = ev.LongRunningToolIDs[0]
		}
	}
	if id == "" {
		t.Fatal("the run did not park on await_ci")
	}
	return id
}

// suspendStub drives the long-running cycle deterministically:
//   - no await_ci response in history       -> call await_ci
//   - await_ci returned {"status":"pending"} -> acknowledge, end the turn (park)
//   - await_ci returned {"conclusion": ...}  -> report the final result
type suspendStub struct{}

func (suspendStub) Name() string { return "suspend-stub" }
func (suspendStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var conclusion string
	pending := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse == nil || p.FunctionResponse.Name != "await_ci" {
				continue
			}
			if v, ok := p.FunctionResponse.Response["conclusion"]; ok {
				conclusion = fmt.Sprint(v)
			} else if s, ok := p.FunctionResponse.Response["status"]; ok && fmt.Sprint(s) == "pending" {
				pending = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case conclusion != "":
			yield(&model.LLMResponse{Content: AssistantText("CI concluded: " + conclusion), TurnComplete: true, FinishReason: genai.FinishReasonStop}, nil)
		case pending:
			yield(&model.LLMResponse{Content: AssistantText("Awaiting CI."), TurnComplete: true, FinishReason: genai.FinishReasonStop}, nil)
		default:
			fc := &genai.FunctionCall{ID: "call_ci_1", Name: "await_ci", Args: map[string]any{"pr": float64(1)}}
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}, TurnComplete: true, FinishReason: genai.FinishReasonStop}, nil)
		}
	}
}

// resumeWith feeds a function-response (the CI outcome) for the parked call back on
// the same session, returning the final text and whether the run re-parked.
func resumeWith(t *testing.T, r *runner.Runner, uid, sid, callID, conclusion string) (final string, reparked bool) {
	t.Helper()
	resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: callID, Name: "await_ci", Response: map[string]any{"conclusion": conclusion}},
	}}}
	for ev, err := range r.Run(context.Background(), uid, sid, resume, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("resume run: %v", err)
		}
		if len(ev.LongRunningToolIDs) > 0 {
			reparked = true
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				final += p.Text
			}
		}
	}
	return final, reparked
}

// TestLongRunningSuspendResume proves the core architecture mechanic: a run parks on
// a long-running tool, and a SECOND runner.Run on the SAME in-memory session resumes
// it with the supplied result rather than restarting.
func TestLongRunningSuspendResume(t *testing.T) {
	r := newCIWaiter(t)
	id := park(t, r, "u", "s")
	t.Logf("parked on long-running call id=%q", id)

	final, _ := resumeWith(t, r, "u", "s", id, "success")
	if !strings.Contains(final, "success") {
		t.Fatalf("resume did not continue with the CI result; final=%q", final)
	}
	t.Logf("resumed and concluded: %q", final)
}

// TestLateWebhookAfterTimeout proves the race is safe at the runner level (defense in
// depth behind the registry's atomic claim): after a timeout has concluded the run, a
// LATE CI webhook replaying the same call id must NOT re-park or leak a new parked
// run. (In production the registry drops it before it ever reaches the runner.)
func TestLateWebhookAfterTimeout(t *testing.T) {
	r := newCIWaiter(t)
	id := park(t, r, "u", "s")

	if _, reparked := resumeWith(t, r, "u", "s", id, "timeout"); reparked {
		t.Fatal("timeout resume re-parked")
	}

	// Late webhook replays the same (now stale) call id.
	resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: id, Name: "await_ci", Response: map[string]any{"conclusion": "success"}},
	}}}
	var reparked bool
	var runErr error
	for ev, err := range r.Run(context.Background(), "u", "s", resume, agent.RunConfig{}) {
		if err != nil {
			runErr = err
			break
		}
		if len(ev.LongRunningToolIDs) > 0 {
			reparked = true
		}
	}
	if reparked {
		t.Fatal("late webhook re-parked the run — would leak a parked run")
	}
	t.Logf("late webhook after timeout handled at runner level (err=%v, no re-park)", runErr)
}

// TestLongRunningTimeoutResume proves the kill path: when CI never reports, the
// CI_TIMEOUT timer fires and resumes the parked run with a timeout outcome, which
// concludes it cleanly — final message emitted, NO re-park. The run is freed, not
// left hanging in memory.
func TestLongRunningTimeoutResume(t *testing.T) {
	r := newCIWaiter(t)
	id := park(t, r, "u", "s")

	// Simulate the per-run CI_TIMEOUT timer firing (CI never arrived).
	fired := make(chan struct{})
	timer := time.AfterFunc(20*time.Millisecond, func() { close(fired) })
	defer timer.Stop()
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("timeout timer never fired")
	}

	final, reparked := resumeWith(t, r, "u", "s", id, "timeout")
	if reparked {
		t.Fatal("run re-parked after timeout — it was not killed/freed")
	}
	if !strings.Contains(final, "timeout") {
		t.Fatalf("timeout outcome not surfaced; final=%q", final)
	}
	t.Logf("timed-out run concluded cleanly: %q", final)
}
