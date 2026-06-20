package root

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"

	"github.com/jkjamies/automation-agent/internal/ingest"
)

func env(kind ingest.Kind) ingest.Envelope {
	return ingest.New(kind, "test", nil, time.Unix(1, 0))
}

func TestDispatchRoutesByKind(t *testing.T) {
	d := NewDispatcher(nil)
	var got ingest.Kind
	d.Register(ingest.KindCronDaily, func(_ context.Context, e ingest.Envelope) error {
		got = e.Kind
		return nil
	})

	if !d.Handles(ingest.KindCronDaily) {
		t.Error("Handles should report registered kind")
	}
	if err := d.Dispatch(context.Background(), env(ingest.KindCronDaily)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got != ingest.KindCronDaily {
		t.Errorf("handler got kind %q", got)
	}
}

func TestDispatchUnhandledIsNoOp(t *testing.T) {
	d := NewDispatcher(nil)
	if d.Handles(ingest.KindLint) {
		t.Error("nothing registered yet")
	}
	if err := d.Dispatch(context.Background(), env(ingest.KindLint)); err != nil {
		t.Errorf("unhandled kind should be a no-op, got %v", err)
	}
}

func TestDispatchPropagatesHandlerError(t *testing.T) {
	d := NewDispatcher(nil)
	d.Register(ingest.KindCI, func(context.Context, ingest.Envelope) error {
		return errors.New("handler failed")
	})
	if err := d.Dispatch(context.Background(), env(ingest.KindCI)); err == nil {
		t.Fatal("expected handler error to propagate")
	}
}

// trivialAgent is a code agent that emits one event, used to build a real runner
// without an LLM.
func trivialAgent(t *testing.T) agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Name: "trivial",
		Run: func(_ agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				yield(&session.Event{Author: "trivial"}, nil)
			}
		},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestBuildRootDispatcherWithSummary(t *testing.T) {
	d, err := BuildRootDispatcher(Deps{SummaryAgent: trivialAgent(t)})
	if err != nil {
		t.Fatalf("BuildRootDispatcher: %v", err)
	}
	if !d.Handles(ingest.KindCronDaily) || !d.Handles(ingest.KindCronWeekly) {
		t.Error("cron kinds should route to the summary workflow")
	}
	if err := d.Dispatch(context.Background(), env(ingest.KindCronDaily)); err != nil {
		t.Fatalf("dispatch cron: %v", err)
	}
}

func TestBuildRootDispatcherWithoutSummary(t *testing.T) {
	d, err := BuildRootDispatcher(Deps{SummaryAgent: nil})
	if err != nil {
		t.Fatalf("BuildRootDispatcher: %v", err)
	}
	if d.Handles(ingest.KindCronDaily) {
		t.Error("no summary agent -> cron kinds unhandled")
	}
}
