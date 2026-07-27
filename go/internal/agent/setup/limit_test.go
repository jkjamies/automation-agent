package setup

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
)

// concurrencyProbe records the highest number of generations in flight at once, which is
// the only property the limiter actually promises.
type concurrencyProbe struct {
	inFlight atomic.Int64
	peak     atomic.Int64
	hold     time.Duration
}

func (p *concurrencyProbe) Name() string { return "probe" }

func (p *concurrencyProbe) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := p.inFlight.Add(1)
		for {
			peak := p.peak.Load()
			if n <= peak || p.peak.CompareAndSwap(peak, n) {
				break
			}
		}
		time.Sleep(p.hold) // overlap window: without a limit these all pile up here
		p.inFlight.Add(-1)
		yield(FinalTextResponse("ok"), nil)
	}
}

// drain consumes a response iterator to completion, which is what releases the slot.
func drain(seq iter.Seq2[*model.LLMResponse, error]) {
	for range seq { //nolint:revive // draining is the point
	}
}

func TestLLMLimiterBoundsConcurrency(t *testing.T) {
	const limit, callers = 3, 20
	probe := &concurrencyProbe{hold: 5 * time.Millisecond}
	limited := NewLLMLimiter(limit).Wrap(probe)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drain(limited.GenerateContent(context.Background(), &model.LLMRequest{}, false))
		}()
	}
	wg.Wait()

	if peak := probe.peak.Load(); peak > limit {
		t.Errorf("peak concurrency = %d, want at most %d", peak, limit)
	}
	// And it must not have serialized everything — a limiter that admits one at a time would
	// pass the check above while destroying throughput.
	if peak := probe.peak.Load(); peak < 2 {
		t.Errorf("peak concurrency = %d; the limiter should still allow real parallelism", peak)
	}
}

// The slot is held for the whole generation, not just its start. A streamed response
// occupies the backend until its last token, so releasing early would let unbounded
// generations overlap and quietly defeat the limit.
func TestLLMLimiterHoldsSlotForWholeGeneration(t *testing.T) {
	probe := &concurrencyProbe{hold: 20 * time.Millisecond}
	limited := NewLLMLimiter(1).Wrap(probe)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drain(limited.GenerateContent(context.Background(), &model.LLMRequest{}, false))
		}()
	}
	wg.Wait()

	if peak := probe.peak.Load(); peak != 1 {
		t.Errorf("peak concurrency = %d, want exactly 1 — the slot was released mid-generation", peak)
	}
}

// A caller whose context expires while queued gets its error rather than blocking. Under a
// burst the queue can be long, and a dispatch deadline or a shutdown must still cut through.
func TestLLMLimiterRespectsContextWhileQueued(t *testing.T) {
	probe := &concurrencyProbe{hold: 50 * time.Millisecond}
	limited := NewLLMLimiter(1).Wrap(probe)

	// Occupy the only slot.
	started := make(chan struct{})
	go func() {
		close(started)
		drain(limited.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	var gotErr error
	for _, err := range limited.GenerateContent(ctx, &model.LLMRequest{}, false) {
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("a queued caller whose context expired should surface the context error")
	}
}

// A non-positive limit means "unbounded", and Wrap passes the model through untouched so
// callers never have to special-case it.
func TestLLMLimiterDisabled(t *testing.T) {
	probe := &concurrencyProbe{}
	if got := NewLLMLimiter(0).Wrap(probe); got != model.LLM(probe) {
		t.Errorf("a zero limit should return the model unwrapped, got %T", got)
	}
	var nilLimiter *LLMLimiter
	if got := nilLimiter.Wrap(probe); got != model.LLM(probe) {
		t.Errorf("a nil limiter should return the model unwrapped, got %T", got)
	}
	if got := NewLLMLimiter(4).Wrap(nil); got != nil {
		t.Errorf("wrapping a nil model should stay nil, got %T", got)
	}
}
