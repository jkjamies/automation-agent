package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"automation-agent/internal/ingest"
)

func TestInProcessDispatches(t *testing.T) {
	var mu sync.Mutex
	var got ingest.Envelope
	done := make(chan struct{})
	p := NewInProcess(func(_ context.Context, e ingest.Envelope) error {
		mu.Lock()
		got = e
		mu.Unlock()
		close(done)
		return nil
	}, nil, 4)

	if err := p.Enqueue(context.Background(), ingest.New(ingest.KindLint, "webhook:/lint", []byte("x"), time.Unix(0, 0))); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not run")
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Kind != ingest.KindLint {
		t.Errorf("kind = %q, want lint", got.Kind)
	}
}

// A dispatch error is logged, not returned (the webhook response has already gone out),
// so Enqueue still succeeds.
func TestInProcessSwallowsDispatchError(t *testing.T) {
	done := make(chan struct{})
	p := NewInProcess(func(context.Context, ingest.Envelope) error {
		close(done)
		return errors.New("boom")
	}, nil, 1)
	if err := p.Enqueue(context.Background(), ingest.New(ingest.KindCI, "s", nil, time.Unix(0, 0))); err != nil {
		t.Fatalf("Enqueue should not surface dispatch error: %v", err)
	}
	<-done
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A non-positive maxConcurrent and a nil logger fall back to defaults (the constructor
// must not panic and the pool must still dispatch).
func TestInProcessConstructorDefaults(t *testing.T) {
	done := make(chan struct{})
	p := NewInProcess(func(context.Context, ingest.Envelope) error { close(done); return nil }, nil, 0)
	if cap(p.sem) != DefaultMaxConcurrent {
		t.Errorf("sem cap = %d, want default %d", cap(p.sem), DefaultMaxConcurrent)
	}
	_ = p.Enqueue(context.Background(), ingest.New(ingest.KindLint, "s", nil, time.Unix(0, 0)))
	<-done
}

// Enqueue honors a cancelled context while waiting for a pool slot (backpressure path).
func TestInProcessEnqueueRespectsCancelledContext(t *testing.T) {
	// Pool size 1, kept occupied by a blocked dispatch, so the next Enqueue must wait.
	release := make(chan struct{})
	p := NewInProcess(func(context.Context, ingest.Envelope) error { <-release; return nil }, nil, 1)
	_ = p.Enqueue(context.Background(), ingest.New(ingest.KindCI, "s", nil, time.Unix(0, 0)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Enqueue(ctx, ingest.New(ingest.KindCI, "s", nil, time.Unix(0, 0))); err == nil {
		t.Error("Enqueue with a cancelled context should return its error")
	}
	close(release)
	_ = p.Close()
}

// Close drains in-flight work: a dispatch still running when Close is called completes
// before Close returns.
func TestInProcessCloseDrains(t *testing.T) {
	release := make(chan struct{})
	var finished bool
	p := NewInProcess(func(context.Context, ingest.Envelope) error {
		<-release
		finished = true
		return nil
	}, nil, 1)
	_ = p.Enqueue(context.Background(), ingest.New(ingest.KindCronDaily, "s", nil, time.Unix(0, 0)))

	closed := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closed)
	}()
	// Close must still be waiting while the dispatch is blocked.
	select {
	case <-closed:
		t.Fatal("Close returned before in-flight work finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-closed
	if !finished {
		t.Error("in-flight dispatch did not finish before Close returned")
	}
}

// After Close, Enqueue refuses new work rather than launching a goroutine the drain has
// already stopped waiting for (which would also race wg.Add against Close's wg.Wait).
func TestInProcessEnqueueAfterCloseIsRejected(t *testing.T) {
	var ran bool
	p := NewInProcess(func(context.Context, ingest.Envelope) error { ran = true; return nil }, nil, 1)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Enqueue(context.Background(), ingest.New(ingest.KindCI, "s", nil, time.Unix(0, 0))); err == nil {
		t.Error("Enqueue after Close should return an error")
	}
	// Give any (incorrectly) launched goroutine a chance to run before asserting it did not.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if ran {
		t.Error("dispatch ran for work enqueued after Close")
	}
}
