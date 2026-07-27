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
	// hold and release are alternative ways to keep a generation in flight. hold sleeps for a
	// fixed duration; release blocks until the test closes it. Prefer release wherever the test
	// needs the generation to still be running at a known point — a sleep only makes that
	// likely, and "likely" is what makes a test flaky on a loaded CI runner.
	hold    time.Duration
	release chan struct{}
	// entered, when non-nil, receives once per generation at the moment it starts — which is
	// *after* the limiter admitted it. That makes "the slot is taken" an observed fact. A
	// signal sent from the calling goroutine instead would only prove the goroutine was
	// scheduled, which is not the same thing and is exactly what used to flake here.
	entered chan struct{}
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
		if p.entered != nil {
			p.entered <- struct{}{}
		}
		if p.release != nil {
			<-p.release // stay in flight until the test says otherwise
		} else {
			time.Sleep(p.hold) // overlap window: without a limit these all pile up here
		}
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
	// Deliberately no lower-bound assertion here: whether these 20 callers actually overlap is
	// the scheduler's business, so asserting it would be a coin flip on a loaded runner. That
	// the limiter still permits real parallelism is proven deterministically below.
}

// A limiter that admitted one caller at a time would satisfy the upper bound above while
// destroying throughput, so prove the parallelism directly: exactly `limit` generations must be
// able to sit in flight simultaneously. The release channel makes this a fact rather than a race
// the scheduler usually — but not always — wins.
func TestLLMLimiterAdmitsUpToTheLimitAtOnce(t *testing.T) {
	const limit = 3
	release := make(chan struct{})
	defer close(release)
	probe := &concurrencyProbe{entered: make(chan struct{}, limit), release: release}
	limited := NewLLMLimiter(limit).Wrap(probe)

	for i := 0; i < limit; i++ {
		go drain(limited.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	}
	for i := 0; i < limit; i++ {
		select {
		case <-probe.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d generations were admitted concurrently", i, limit)
		}
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
	// Occupy the only slot, and hold it open until this test is done rather than for a fixed
	// duration the queued caller might outlive.
	release := make(chan struct{})
	defer close(release)
	probe := &concurrencyProbe{entered: make(chan struct{}, 1), release: release}
	limited := NewLLMLimiter(1).Wrap(probe)

	go drain(limited.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	// Wait for the probe to report it is running. The limiter admits before the probe starts,
	// so this proves the slot is held; signalling before calling GenerateContent (as this test
	// used to) proved only that the goroutine had been scheduled, and when the caller below won
	// that race it found the slot free and never queued at all.
	<-probe.entered

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
