package fixflow

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func noTimeout(string) {}

func TestRegistryResolveOnce(t *testing.T) {
	r := newRunRegistry()
	r.Park("o/r#1", &ParkedRun{SessionID: "s", CallID: "c"}, time.Hour, noTimeout)
	if r.Len() != 1 {
		t.Fatalf("len = %d", r.Len())
	}

	run, ok := r.Resolve("o/r#1")
	if !ok || run.CallID != "c" {
		t.Fatalf("first resolve = %+v, %v", run, ok)
	}
	if _, ok := r.Resolve("o/r#1"); ok {
		t.Error("second resolve should find nothing (already claimed)")
	}
	if r.Len() != 0 {
		t.Errorf("len after resolve = %d", r.Len())
	}
}

func TestRegistryLateResolveNoop(t *testing.T) {
	r := newRunRegistry()
	if _, ok := r.Resolve("never/parked#9"); ok {
		t.Error("resolving an unparked PR should no-op")
	}
}

// TestRegistryTimeoutClaims proves the timer path claims the run, and a webhook that
// arrives afterward (late) finds nothing.
func TestRegistryTimeoutClaims(t *testing.T) {
	r := newRunRegistry()
	claimed := make(chan bool, 1)
	r.Park("o/r#2", &ParkedRun{SessionID: "s", CallID: "c"}, 10*time.Millisecond, func(pr string) {
		_, ok := r.Resolve(pr)
		claimed <- ok
	})

	select {
	case ok := <-claimed:
		if !ok {
			t.Fatal("timeout fired but failed to claim the parked run")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout never fired")
	}
	// Late webhook after the timeout claimed it.
	if _, ok := r.Resolve("o/r#2"); ok {
		t.Error("late webhook after timeout should find nothing")
	}
}

// TestRegistryConcurrentResolveExactlyOne proves the atomic claim under contention:
// when many callers (duplicate webhooks + a timer) race to resolve one parked run,
// exactly one wins.
func TestRegistryConcurrentResolveExactlyOne(t *testing.T) {
	r := newRunRegistry()
	r.Park("o/r#3", &ParkedRun{SessionID: "s", CallID: "c"}, time.Hour, noTimeout)

	const racers = 50
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := r.Resolve("o/r#3"); ok {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one resolver must win, got %d", wins)
	}
}

// TestRegistryRepark proves a retry re-parks under the same PR key (stopping the old
// timer), and only the latest parking is resolvable.
func TestRegistryRepark(t *testing.T) {
	r := newRunRegistry()
	r.Park("o/r#4", &ParkedRun{SessionID: "s", CallID: "c1", Attempts: 1}, time.Hour, noTimeout)
	r.Park("o/r#4", &ParkedRun{SessionID: "s", CallID: "c2", Attempts: 2}, time.Hour, noTimeout)
	if r.Len() != 1 {
		t.Fatalf("re-park should replace, len = %d", r.Len())
	}
	run, ok := r.Resolve("o/r#4")
	if !ok || run.CallID != "c2" || run.Attempts != 2 {
		t.Fatalf("resolve = %+v, %v (want latest c2/2)", run, ok)
	}
}
