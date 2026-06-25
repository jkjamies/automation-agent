package root

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"

	"automation-agent/internal/ingest"
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
	d, err := BuildRootDispatcher(Deps{SummaryDaily: trivialAgent(t)})
	if err != nil {
		t.Fatalf("BuildRootDispatcher: %v", err)
	}
	if !d.Handles(ingest.KindCronDaily) {
		t.Error("the daily cron kind should route to the summary workflow")
	}
	if err := d.Dispatch(context.Background(), env(ingest.KindCronDaily)); err != nil {
		t.Fatalf("dispatch cron: %v", err)
	}
}

func TestBuildRootDispatcherFixHandlers(t *testing.T) {
	called := map[ingest.Kind]bool{}
	mark := func(_ context.Context, e ingest.Envelope) error { called[e.Kind] = true; return nil }
	d, err := BuildRootDispatcher(Deps{
		LintKickoff:     mark,
		CoverageKickoff: mark,
		CIResume:        mark,
	})
	if err != nil {
		t.Fatalf("BuildRootDispatcher: %v", err)
	}
	if !d.Handles(ingest.KindLint) || !d.Handles(ingest.KindCoverage) || !d.Handles(ingest.KindCI) {
		t.Fatal("lint/coverage/ci kinds should be registered")
	}
	for _, k := range []ingest.Kind{ingest.KindLint, ingest.KindCoverage, ingest.KindCI} {
		_ = d.Dispatch(context.Background(), env(k))
		if !called[k] {
			t.Errorf("handler for %s not invoked", k)
		}
	}
}

func TestBuildRootDispatcherWithoutSummary(t *testing.T) {
	d, err := BuildRootDispatcher(Deps{})
	if err != nil {
		t.Fatalf("BuildRootDispatcher: %v", err)
	}
	if d.Handles(ingest.KindCronDaily) {
		t.Error("no summary agent -> the daily cron kind is unhandled")
	}
}
