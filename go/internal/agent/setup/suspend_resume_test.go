package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// newCIWaiterAgent builds the minimal parking workflow: a single await node that pauses
// for the CI result on its first pass, then routes to a conclude node that reports the
// conclusion. It is the setup-level proof of the pause/resume mechanic the fix loop
// runs on.
func newCIWaiterAgent(t *testing.T) agent.Agent {
	t.Helper()
	rerun := true
	await := workflow.NewEmittingFunctionNode[any, any]("await_ci",
		func(nc agent.Context, _ any, emit func(*session.Event) error) (any, error) {
			reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{
				InterruptID: "await_ci-" + nc.InvocationID(),
				Message:     "Open the PR and wait for CI to report.",
			})
			if err != nil {
				return nil, err
			}
			ev := session.NewEvent(nc, nc.InvocationID())
			ev.Output = reply
			ev.Routes = []string{"concluded"}
			if err := emit(ev); err != nil {
				return nil, err
			}
			return nil, nil
		}, workflow.NodeConfig{RerunOnResume: &rerun})
	conclude := workflow.NewFunctionNode[any, *genai.Content]("conclude",
		func(_ agent.Context, input any) (*genai.Content, error) {
			conclusion := "unknown"
			if m, ok := input.(map[string]any); ok {
				conclusion = fmt.Sprint(m["conclusion"])
			}
			return AssistantText("CI concluded: " + conclusion), nil
		}, workflow.NodeConfig{})
	a, err := workflowagent.New(workflowagent.Config{
		Name:        "ci-waiter",
		Description: "parks awaiting a CI result, then reports it",
		Edges: []workflow.Edge{
			{From: workflow.Start, To: await},
			{From: await, To: conclude, Route: workflow.Default},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// newCIWaiter builds the parking agent + a shared in-memory runner used by the
// suspend/resume tests.
func newCIWaiter(t *testing.T) *runner.Runner {
	t.Helper()
	r, err := runner.New(runner.Config{
		AppName: "susp", Agent: newCIWaiterAgent(t),
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// park runs until the agent pauses on the request-input park and returns its interrupt id.
func park(t *testing.T, r *runner.Runner, uid, sid string) string {
	t.Helper()
	var id string
	for ev, err := range r.Run(context.Background(), uid, sid, userText("fix coverage"), agent.RunConfig{}) {
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

// resumeWith feeds a request-input response (the CI outcome) for the parked interrupt
// back on the same session, returning the final text and whether the run re-parked.
func resumeWith(t *testing.T, r *runner.Runner, uid, sid, callID, conclusion string) (final string, reparked bool) {
	t.Helper()
	resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID:       callID,
			Name:     workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": map[string]any{"conclusion": conclusion}},
		},
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

// TestLongRunningSuspendResume proves the core architecture mechanic: a run parks on a
// request-input pause, and a SECOND runner.Run on the SAME in-memory session resumes it
// with the supplied result rather than restarting.
func TestLongRunningSuspendResume(t *testing.T) {
	r := newCIWaiter(t)
	id := park(t, r, "u", "s")
	t.Logf("parked on interrupt id=%q", id)

	final, _ := resumeWith(t, r, "u", "s", id, "success")
	if !strings.Contains(final, "success") {
		t.Fatalf("resume did not continue with the CI result; final=%q", final)
	}
	t.Logf("resumed and concluded: %q", final)
}

// TestLateWebhookAfterTimeout proves the race is safe at the engine level (defense in
// depth behind the ParkStore's atomic claim): after a timeout has concluded the run, a
// LATE CI webhook replaying the same interrupt id must NOT re-park or leak a new parked
// run — the engine recognizes the interrupt as already resolved and no-ops the turn.
// (In production the ParkStore claim drops it before it ever reaches the runner.)
func TestLateWebhookAfterTimeout(t *testing.T) {
	r := newCIWaiter(t)
	id := park(t, r, "u", "s")

	if _, reparked := resumeWith(t, r, "u", "s", id, "timeout"); reparked {
		t.Fatal("timeout resume re-parked")
	}

	// Late webhook replays the same (now resolved) interrupt id.
	final, reparked := resumeWith(t, r, "u", "s", id, "success")
	if reparked {
		t.Fatal("late webhook re-parked the run — would leak a parked run")
	}
	t.Logf("late webhook after timeout no-oped at the engine level (final=%q, no re-park)", final)
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
