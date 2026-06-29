package reviewer

import (
	"context"
	"testing"
)

// When disabled (the default), Kickoff acknowledges the event and does no work.
func TestKickoffDisabledNoOp(t *testing.T) {
	e := NewEngine(Deps{Enabled: false})
	if err := e.Kickoff(context.Background(), []byte(`{"action":"opened"}`)); err != nil {
		t.Fatalf("disabled Kickoff returned error: %v", err)
	}
}

// When enabled, Kickoff accepts the event (the diff fetch / fan-out / publish land later).
func TestKickoffEnabled(t *testing.T) {
	e := NewEngine(Deps{Enabled: true})
	if err := e.Kickoff(context.Background(), []byte(`{"action":"synchronize"}`)); err != nil {
		t.Fatalf("enabled Kickoff returned error: %v", err)
	}
}

// NewEngine tolerates a nil logger (falls back to the default) rather than panicking.
func TestNewEngineNilLogger(t *testing.T) {
	e := NewEngine(Deps{Enabled: true, Log: nil})
	if err := e.Kickoff(context.Background(), nil); err != nil {
		t.Fatalf("Kickoff with default logger returned error: %v", err)
	}
}
