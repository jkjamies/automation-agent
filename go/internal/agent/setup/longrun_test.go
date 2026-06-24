package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// TestLongRunDriverDeleteSession proves terminal cleanup (E-eager) actually removes the
// stored session, so a durable backend does not leak completed runs.
func TestLongRunDriverDeleteSession(t *testing.T) {
	apply, await, _, _ := lrTools(t)
	model := NewSequencerModel(SequencerConfig{Action: "apply", Wait: "await"})
	ag, err := llmagent.New(llmagent.Config{
		Name: "lr", Model: model, Instruction: "apply then await", Tools: []tool.Tool{apply, await},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := session.InMemoryService()
	d, err := NewLongRunDriver("lr-app", "u", ag, sess)
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

type lrEmpty struct{}

type lrArgs struct {
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha"`
}

// lrTools builds an apply/await tool pair plus a pointer to the apply call counter and a
// switch to force apply to fail, for driving the sequencer through its states.
func lrTools(t *testing.T) (apply, await tool.Tool, calls *int, fail *bool) {
	t.Helper()
	n := 0
	boom := false
	a, err := functiontool.New(functiontool.Config{Name: "apply", Description: "apply a fix"},
		func(_ tool.Context, _ lrEmpty) (map[string]any, error) {
			n++
			if boom {
				return nil, fmt.Errorf("apply boom")
			}
			return map[string]any{"pr_number": 7, "head_sha": "abc"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	w, err := functiontool.New(functiontool.Config{Name: "await", Description: "await CI", IsLongRunning: true},
		func(_ tool.Context, _ lrArgs) (map[string]any, error) {
			return map[string]any{"status": "pending"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return a, w, &n, &boom
}

func newLRDriver(t *testing.T, apply, await tool.Tool) *LongRunDriver {
	t.Helper()
	model := NewSequencerModel(SequencerConfig{
		Action:    "apply",
		Wait:      "await",
		RetryWhen: func(r map[string]any) bool { return fmt.Sprint(r["conclusion"]) == "failure" },
	})
	ag, err := llmagent.New(llmagent.Config{
		Name: "lr", Model: model, Instruction: "apply then await",
		Tools: []tool.Tool{apply, await},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewLongRunDriver("lr-app", "u", ag, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestLongRunDriverLoop drives the full Start → park → resume(failure) → re-park →
// resume(success) cycle and asserts apply runs once per attempt and the loop concludes.
func TestLongRunDriverLoop(t *testing.T) {
	apply, await, calls, _ := lrTools(t)
	d := newLRDriver(t, apply, await)
	ctx := context.Background()

	start, err := d.Start(ctx, "s1", "go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.ParkedCallID == "" {
		t.Fatal("Start did not park on await")
	}
	if got := start.ToolResponses["apply"]["pr_number"]; fmt.Sprint(got) != "7" {
		t.Errorf("apply pr_number = %v, want 7", got)
	}
	if *calls != 1 {
		t.Errorf("apply calls after start = %d, want 1", *calls)
	}

	// CI failed → resume should re-apply and re-park.
	retry, err := d.Resume(ctx, "s1", start.ParkedCallID, "await", map[string]any{"conclusion": "failure"})
	if err != nil {
		t.Fatalf("Resume failure: %v", err)
	}
	if retry.ParkedCallID == "" {
		t.Fatal("failure resume did not re-park")
	}
	if retry.ParkedCallID == start.ParkedCallID {
		t.Error("re-park should use a fresh call id")
	}
	if *calls != 2 {
		t.Errorf("apply calls after retry = %d, want 2", *calls)
	}

	// CI passed → resume should conclude without re-parking.
	done, err := d.Resume(ctx, "s1", retry.ParkedCallID, "await", map[string]any{"conclusion": "success"})
	if err != nil {
		t.Fatalf("Resume success: %v", err)
	}
	if done.ParkedCallID != "" {
		t.Error("success resume should not re-park")
	}
	if *calls != 2 {
		t.Errorf("apply must not run again on success, calls = %d", *calls)
	}
	if !strings.Contains(done.Final, "done") {
		t.Errorf("final = %q, want it to conclude", done.Final)
	}
}

// TestLongRunDriverApplyError proves an apply failure surfaces (as an "error" response
// and final text) and does not park a run.
func TestLongRunDriverApplyError(t *testing.T) {
	apply, await, _, fail := lrTools(t)
	*fail = true
	d := newLRDriver(t, apply, await)

	res, err := d.Start(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.ParkedCallID != "" {
		t.Error("a failed apply must not park")
	}
	if _, ok := res.ToolResponses["apply"]["error"]; !ok {
		t.Errorf("expected an error response, got %+v", res.ToolResponses["apply"])
	}
	if !strings.Contains(res.Final, "failed") {
		t.Errorf("final = %q, want it to report the failure", res.Final)
	}
}

// TestSequencerDecide exercises the pure decision logic over crafted histories.
func TestSequencerDecide(t *testing.T) {
	s := &sequencer{cfg: SequencerConfig{
		Action:    "apply",
		Wait:      "await",
		RetryWhen: func(r map[string]any) bool { return fmt.Sprint(r["conclusion"]) == "failure" },
	}}

	fcName := func(resp *genai.Content) (name, text string) {
		for _, p := range resp.Parts {
			if p.FunctionCall != nil {
				return p.FunctionCall.Name, ""
			}
			text += p.Text
		}
		return "", text
	}
	resp := func(name string, body map[string]any) *genai.Content {
		return &genai.Content{Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: name, Response: body}}}}
	}

	// No history → call apply.
	if name, _ := fcName(s.decide(nil).Content); name != "apply" {
		t.Errorf("empty history should call apply, got %q", name)
	}
	// apply ok → call await.
	if name, _ := fcName(s.decide([]*genai.Content{resp("apply", map[string]any{"pr_number": 7})}).Content); name != "await" {
		t.Errorf("apply success should call await, got %q", name)
	}
	// apply error → conclude.
	if name, text := fcName(s.decide([]*genai.Content{resp("apply", map[string]any{"error": "x"})}).Content); name != "" || !strings.Contains(text, "failed") {
		t.Errorf("apply error should conclude, got name=%q text=%q", name, text)
	}
	// await failure → retry apply.
	if name, _ := fcName(s.decide([]*genai.Content{resp("await", map[string]any{"conclusion": "failure"})}).Content); name != "apply" {
		t.Errorf("await failure should retry apply, got %q", name)
	}
	// await success → conclude.
	if name, text := fcName(s.decide([]*genai.Content{resp("await", map[string]any{"conclusion": "success"})}).Content); name != "" || text == "" {
		t.Errorf("await success should conclude, got name=%q text=%q", name, text)
	}
}
