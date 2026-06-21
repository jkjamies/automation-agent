package setup

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

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

// TestLongRunningSuspendResume proves the core architecture mechanic: a run parks on
// a long-running tool, and a SECOND runner.Run on the SAME in-memory session resumes
// it with the supplied result rather than restarting.
func TestLongRunningSuspendResume(t *testing.T) {
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

	// One in-memory session service shared across BOTH runs — this is what must carry
	// the parked run.
	sessions := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: "susp", Agent: a, SessionService: sessions, AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	const uid, sid = "u", "s"

	// --- suspend ---
	var longRunningID string
	for ev, err := range r.Run(ctx, uid, sid, UserText("fix coverage"), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("suspend run: %v", err)
		}
		if len(ev.LongRunningToolIDs) > 0 {
			longRunningID = ev.LongRunningToolIDs[0]
		}
	}
	if longRunningID == "" {
		t.Fatal("no long-running tool id captured — the run did not park on await_ci")
	}
	t.Logf("parked on long-running call id=%q", longRunningID)

	// --- resume: feed the final CI result back on the SAME session ---
	resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: longRunningID, Name: "await_ci", Response: map[string]any{"conclusion": "success"}},
	}}}
	var final string
	for ev, err := range r.Run(ctx, uid, sid, resume, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("resume run: %v", err)
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				final += p.Text
			}
		}
	}
	if !strings.Contains(final, "success") {
		t.Fatalf("resume did not continue with the CI result; final=%q", final)
	}
	t.Logf("resumed and concluded: %q", final)
}
