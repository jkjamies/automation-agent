package setup

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"
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
