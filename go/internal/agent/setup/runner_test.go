package setup

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestNewRunnerAndDrive(t *testing.T) {
	echo, err := agent.New(agent.Config{
		Name: "echo",
		Run: func(_ agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				yield(TextEvent("echo", "hello", map[string]any{"k": "v"}), nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	r, err := NewRunner("test-app", echo)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := Drive(context.Background(), r, "u", "s", "go"); err != nil {
		t.Fatalf("Drive: %v", err)
	}
}

// SSE streaming is required: without it Ollama buffers the whole answer before sending
// any bytes, turning the transport's first-chunk timeout into a cap on total generation
// (a long code change on slow hardware then times out). Guard the invariant so a future
// edit can't silently drop back to non-streaming.
func TestStreamingRunConfigUsesSSE(t *testing.T) {
	if got := streamingRunConfig().StreamingMode; got != agent.StreamingModeSSE {
		t.Errorf("streamingRunConfig StreamingMode = %q, want %q", got, agent.StreamingModeSSE)
	}
}

func TestDrivePropagatesError(t *testing.T) {
	boom, _ := agent.New(agent.Config{
		Name: "boom",
		Run: func(_ agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				yield(nil, fmt.Errorf("kaboom"))
			}
		},
	})
	r, _ := NewRunner("test-app", boom)
	if err := Drive(context.Background(), r, "u", "s", "go"); err == nil {
		t.Fatal("expected Drive to propagate the agent error")
	}
}

func TestTextEvent(t *testing.T) {
	ev := TextEvent("author", "body", map[string]any{"key": "val"})
	if ev.Author != "author" {
		t.Errorf("author = %q", ev.Author)
	}
	if ContentText(ev.Content) != "body" {
		t.Errorf("content = %q", ContentText(ev.Content))
	}
	if ev.Actions.StateDelta["key"] != "val" {
		t.Errorf("state delta = %v", ev.Actions.StateDelta)
	}
	if plain := TextEvent("a", "b", nil); plain.Actions.StateDelta != nil {
		t.Error("no state -> nil StateDelta")
	}
}

type mapState map[string]any

func (m mapState) Get(k string) (any, error) {
	v, ok := m[k]
	if !ok {
		return nil, fmt.Errorf("key %q not found", k)
	}
	return v, nil
}

func TestStateString(t *testing.T) {
	s := mapState{"a": "x", "b": 42}
	if StateString(s, "a") != "x" {
		t.Error("string value not returned")
	}
	if StateString(s, "b") != "" {
		t.Error("non-string should yield empty")
	}
	if StateString(s, "missing") != "" {
		t.Error("missing key should yield empty")
	}
}

// usageLLM is a model.LLM whose single final response carries token usage and a model version,
// the way a real adapter's terminal chunk does (Ollama's done chunk, Gemini's aggregated stream).
type usageLLM struct{ in, out int32 }

func (usageLLM) Name() string { return "usage-fake" }

func (m usageLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp := FinalTextResponse("done")
		resp.ModelVersion = "usage-fake-001"
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: m.in, CandidatesTokenCount: m.out}
		yield(resp, nil)
	}
}

// DriveReport attributes each sub-agent's usage, model version, and elapsed time to that agent by
// name, and merges the state keys the sub-agents wrote — the fan-out shape the reviewer reads back.
func TestDriveReportAttributesAgentStats(t *testing.T) {
	newAgent := func(name string, in, out int32) agent.Agent {
		a, err := llmagent.New(llmagent.Config{Name: name, Model: usageLLM{in, out}, InstructionProvider: StaticInstruction("x"), OutputKey: "out:" + name})
		if err != nil {
			t.Fatalf("llmagent.New(%s): %v", name, err)
		}
		return a
	}
	par, err := parallelagent.New(parallelagent.Config{AgentConfig: agent.Config{
		Name: "both", SubAgents: []agent.Agent{newAgent("alpha", 100, 7), newAgent("beta", 30, 2)},
	}})
	if err != nil {
		t.Fatalf("parallelagent.New: %v", err)
	}
	r, err := NewRunner("test-app", par)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	rep, err := DriveReport(context.Background(), r, "u", "s", "go")
	if err != nil {
		t.Fatalf("DriveReport: %v", err)
	}
	for name, want := range map[string]AgentStats{"alpha": {TokensIn: 100, TokensOut: 7}, "beta": {TokensIn: 30, TokensOut: 2}} {
		got, ok := rep.Agents[name]
		if !ok {
			t.Fatalf("no stats for %s: %+v", name, rep.Agents)
		}
		if got.TokensIn != want.TokensIn || got.TokensOut != want.TokensOut || !got.Reported {
			t.Errorf("%s tokens = in %d out %d reported %v, want in %d out %d reported", name, got.TokensIn, got.TokensOut, got.Reported, want.TokensIn, want.TokensOut)
		}
		if got.Model != "usage-fake-001" {
			t.Errorf("%s model = %q, want the reported version", name, got.Model)
		}
		if got.Elapsed <= 0 {
			t.Errorf("%s elapsed = %v, want > 0", name, got.Elapsed)
		}
		if rep.State["out:"+name] != "done" {
			t.Errorf("state[out:%s] = %v, want the agent's output", name, rep.State["out:"+name])
		}
	}
}

// An agent whose responses carry no usage (a code agent, a test double) is still in the report
// with Reported false, so a caller can tell "unknown" from "zero".
func TestDriveReportUnreportedUsage(t *testing.T) {
	echo, _ := agent.New(agent.Config{
		Name: "echo",
		Run: func(_ agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				yield(TextEvent("echo", "hello", nil), nil)
			}
		},
	})
	r, _ := NewRunner("test-app", echo)
	rep, err := DriveReport(context.Background(), r, "u", "s", "go")
	if err != nil {
		t.Fatalf("DriveReport: %v", err)
	}
	st, ok := rep.Agents["echo"]
	if !ok || st.Reported || st.TokensIn != 0 || st.Model != "" {
		t.Errorf("echo stats = %+v (present %v), want present, unreported, empty model", st, ok)
	}
	if rep.Text != "hello" {
		t.Errorf("Text = %q, want hello", rep.Text)
	}
}
