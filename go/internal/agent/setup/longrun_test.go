package setup

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

// lrApplyOutcome is the scripted result of one lrGraph apply activation.
type lrApplyOutcome struct {
	out   map[string]any
	route string
}

// lrGraph builds the canonical parking loop the driver is designed for —
//
//	Start → apply ─"go_wait"→ await (pause) ─"failure"→ apply (cycle)
//	  apply/await ─Default→ conclude
//
// — with the apply node scripted by outcomes (one per activation) and a counter of apply
// activations. It is the test double for a fix-loop workflow: deterministic node bodies,
// real engine routing/pausing.
func lrGraph(t *testing.T, outcomes []lrApplyOutcome, calls *int) agent.Agent {
	t.Helper()
	apply := workflow.NewEmittingFunctionNode[any, any]("apply",
		func(nc agent.Context, _ any, emit func(*session.Event) error) (any, error) {
			i := *calls
			*calls++
			if i >= len(outcomes) {
				t.Errorf("apply activation %d exceeds scripted outcomes", i)
				return nil, fmt.Errorf("unscripted apply activation %d", i)
			}
			ev := session.NewEvent(nc, nc.InvocationID())
			ev.Output = outcomes[i].out
			ev.Routes = []string{outcomes[i].route}
			if err := emit(ev); err != nil {
				return nil, err
			}
			return nil, nil
		}, workflow.NodeConfig{})
	rerun := true
	await := workflow.NewEmittingFunctionNode[any, any]("await",
		func(nc agent.Context, _ any, emit func(*session.Event) error) (any, error) {
			reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{
				InterruptID: "await-" + nc.InvocationID(),
				Message:     "waiting",
			})
			if err != nil {
				return nil, err
			}
			out := map[string]any{NodeOutputKey: "await"}
			route := "conclude"
			if m, ok := reply.(map[string]any); ok {
				for k, v := range m {
					out[k] = v
				}
				if fmt.Sprint(m["conclusion"]) == "failure" {
					route = "failure"
				}
			}
			ev := session.NewEvent(nc, nc.InvocationID())
			ev.Output = out
			ev.Routes = []string{route}
			if err := emit(ev); err != nil {
				return nil, err
			}
			return nil, nil
		}, workflow.NodeConfig{RerunOnResume: &rerun})
	conclude := workflow.NewFunctionNode[any, string]("conclude",
		func(_ agent.Context, _ any) (string, error) { return "done", nil },
		workflow.NodeConfig{})
	ag, err := workflowagent.New(workflowagent.Config{
		Name:        "lr",
		Description: "test parking loop",
		Edges: []workflow.Edge{
			{From: workflow.Start, To: apply},
			{From: apply, To: await, Route: workflow.StringRoute("go_wait")},
			{From: apply, To: conclude, Route: workflow.Default},
			{From: await, To: apply, Route: workflow.StringRoute("failure")},
			{From: await, To: conclude, Route: workflow.Default},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func applyOK() lrApplyOutcome {
	return lrApplyOutcome{
		out:   map[string]any{NodeOutputKey: "apply", "pr_number": 7, "head_sha": "abc"},
		route: "go_wait",
	}
}

// TestLongRunDriverDeleteSession proves terminal cleanup (E-eager) actually removes the
// stored session, so a durable backend does not leak completed runs.
func TestLongRunDriverDeleteSession(t *testing.T) {
	calls := 0
	sess := session.InMemoryService()
	d, err := NewLongRunDriver("lr-app", "u", lrGraph(t, []lrApplyOutcome{applyOK()}, &calls), sess)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := d.Start(ctx, "s1", "go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	get := &session.GetRequest{AppName: "lr-app", UserID: "u", SessionID: "s1"}
	if _, err := sess.Get(ctx, get); err != nil {
		t.Fatalf("session should exist after Start: %v", err)
	}
	if err := d.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := sess.Get(ctx, get); err == nil {
		t.Error("session should be gone after DeleteSession")
	}
	if err := d.DeleteSession(ctx, "s1"); err != nil {
		t.Errorf("deleting a missing session should no-op, got %v", err)
	}
}

// TestLongRunDriverCleanStop proves a terminal apply result (routes to conclude, never to
// the wait node) finishes the run without parking, and its output is readable by tag.
func TestLongRunDriverCleanStop(t *testing.T) {
	calls := 0
	clean := lrApplyOutcome{out: map[string]any{NodeOutputKey: "apply", "clean": true}, route: "conclude"}
	d, err := NewLongRunDriver("lr-app", "u", lrGraph(t, []lrApplyOutcome{clean}, &calls), nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := d.Start(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.ParkedCallID != "" {
		t.Error("a clean (terminal) apply must not park")
	}
	if cleanOut, _ := res.NodeOutput("apply")["clean"].(bool); !cleanOut {
		t.Errorf("expected clean apply output, got %+v", res.NodeOutput("apply"))
	}
	if calls != 1 {
		t.Errorf("apply should run exactly once, ran %d times", calls)
	}
}

// TestLongRunDriverLoop drives the full Start → park → resume(failure) → re-park →
// resume(success) cycle and asserts apply runs once per attempt and the loop concludes.
func TestLongRunDriverLoop(t *testing.T) {
	calls := 0
	d, err := NewLongRunDriver("lr-app", "u", lrGraph(t, []lrApplyOutcome{applyOK(), applyOK()}, &calls), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	start, err := d.Start(ctx, "s1", "go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.ParkedCallID == "" {
		t.Fatal("Start did not park on await")
	}
	if got := start.NodeOutput("apply")["pr_number"]; fmt.Sprint(got) != "7" {
		t.Errorf("apply pr_number = %v, want 7", got)
	}
	if calls != 1 {
		t.Errorf("apply calls after start = %d, want 1", calls)
	}

	// CI failed → resume should re-apply and re-park.
	retry, err := d.Resume(ctx, "s1", start.ParkedCallID, map[string]any{"conclusion": "failure"})
	if err != nil {
		t.Fatalf("Resume failure: %v", err)
	}
	if retry.ParkedCallID == "" {
		t.Fatal("failure resume did not re-park")
	}
	if calls != 2 {
		t.Errorf("apply calls after retry = %d, want 2", calls)
	}

	// CI passed → resume should conclude without re-parking.
	done, err := d.Resume(ctx, "s1", retry.ParkedCallID, map[string]any{"conclusion": "success"})
	if err != nil {
		t.Fatalf("Resume success: %v", err)
	}
	if done.ParkedCallID != "" {
		t.Error("success resume should not re-park")
	}
	if calls != 2 {
		t.Errorf("apply must not run again on success, calls = %d", calls)
	}
	if got := done.NodeOutput("await")["conclusion"]; fmt.Sprint(got) != "success" {
		t.Errorf("await output conclusion = %v, want success", got)
	}
}

// TestLongRunDriverApplyError proves an apply failure surfaces as an "error" output and
// concludes the run without parking it — the caller (not the engine) owns notifying a
// human, so the error must come back as data, not a failed run.
func TestLongRunDriverApplyError(t *testing.T) {
	calls := 0
	boom := lrApplyOutcome{out: map[string]any{NodeOutputKey: "apply", "error": "apply boom"}, route: "conclude"}
	d, err := NewLongRunDriver("lr-app", "u", lrGraph(t, []lrApplyOutcome{boom}, &calls), nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := d.Start(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.ParkedCallID != "" {
		t.Error("a failed apply must not park")
	}
	if _, ok := res.NodeOutput("apply")["error"]; !ok {
		t.Errorf("expected an error output, got %+v", res.NodeOutput("apply"))
	}
}

// TestDriveResultNodeOutput exercises the tag-based selection: latest per tag wins, and
// an unknown tag yields nil.
func TestDriveResultNodeOutput(t *testing.T) {
	res := DriveResult{NodeOutputs: []map[string]any{
		{NodeOutputKey: "apply", "attempt": 1},
		{NodeOutputKey: "await", "conclusion": "failure"},
		{NodeOutputKey: "apply", "attempt": 2},
	}}
	if got := res.NodeOutput("apply")["attempt"]; got != 2 {
		t.Errorf("NodeOutput(apply) attempt = %v, want the latest (2)", got)
	}
	if got := res.NodeOutput("await")["conclusion"]; got != "failure" {
		t.Errorf("NodeOutput(await) conclusion = %v, want failure", got)
	}
	if got := res.NodeOutput("nope"); got != nil {
		t.Errorf("NodeOutput(nope) = %v, want nil", got)
	}
}
