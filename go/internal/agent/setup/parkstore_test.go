package setup

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func parkRec(sid, prKey, callID string, attempts int) ParkRecord {
	return ParkRecord{SessionID: sid, PRKey: prKey, CallID: callID, Attempts: attempts, ParkedAt: time.Now()}
}

// TestParkStoreResolveOnce: a parked record resolves exactly once; a second resolve finds
// nothing, but the per-run record survives for a retry until Delete.
func TestParkStoreResolveOnce(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryParkStore()
	if err := s.Put(ctx, parkRec("sess", "o/r#1", "c", 1)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n, _ := s.ParkedCount(ctx); n != 1 {
		t.Fatalf("parked count = %d, want 1", n)
	}

	run, ok, err := s.ResolveByPRKey(ctx, "o/r#1")
	if err != nil || !ok || run.CallID != "c" {
		t.Fatalf("first resolve = %+v, ok=%v, err=%v", run, ok, err)
	}
	if _, ok, _ := s.ResolveByPRKey(ctx, "o/r#1"); ok {
		t.Error("second resolve should find nothing (already claimed)")
	}
	if n, _ := s.ParkedCount(ctx); n != 0 {
		t.Errorf("parked count after resolve = %d, want 0", n)
	}
	// The per-run record is retained (params survive a retry) until Delete.
	if _, ok, _ := s.Get(ctx, "sess"); !ok {
		t.Error("per-run record should survive a resolve")
	}
	if err := s.Delete(ctx, "sess"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "sess"); ok {
		t.Error("record should be gone after Delete")
	}
}

// TestParkStorePutClearsStaleIndex: re-Putting a session under a different PR key drops
// the old key's index entry, so a resolve on the stale key finds nothing.
func TestParkStorePutClearsStaleIndex(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryParkStore()
	_ = s.Put(ctx, parkRec("sess", "o/r#1", "c", 1))
	_ = s.Put(ctx, parkRec("sess", "o/r#2", "c", 2)) // same session, different PR key
	if _, ok, _ := s.ResolveByPRKey(ctx, "o/r#1"); ok {
		t.Error("the stale PR key should no longer resolve")
	}
	run, ok, _ := s.ResolveByPRKey(ctx, "o/r#2")
	if !ok || run.Attempts != 2 {
		t.Fatalf("resolve on the current key = %+v, %v (want attempts 2)", run, ok)
	}
	if n, _ := s.ParkedCount(ctx); n != 0 {
		t.Errorf("parked count = %d, want 0 (no stale entry left)", n)
	}
}

// TestParkStoreLateResolveNoop: resolving an unparked PR no-ops.
func TestParkStoreLateResolveNoop(t *testing.T) {
	s := NewMemoryParkStore()
	if _, ok, _ := s.ResolveByPRKey(context.Background(), "never/parked#9"); ok {
		t.Error("resolving an unparked PR should no-op")
	}
}

// TestParkStoreRepark: a retry re-parks under the same PR key; only the latest parking is
// resolvable, and the latest CallID/Attempts win.
func TestParkStoreRepark(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryParkStore()
	_ = s.Put(ctx, parkRec("sess", "o/r#4", "c1", 1))
	_ = s.Put(ctx, parkRec("sess", "o/r#4", "c2", 2))
	if n, _ := s.ParkedCount(ctx); n != 1 {
		t.Fatalf("re-park should replace, count = %d", n)
	}
	run, ok, _ := s.ResolveByPRKey(ctx, "o/r#4")
	if !ok || run.CallID != "c2" || run.Attempts != 2 {
		t.Fatalf("resolve = %+v, %v (want latest c2/2)", run, ok)
	}
}

// TestParkStoreConcurrentResolveExactlyOne: under contention (duplicate webhooks + a
// timer) exactly one caller claims a parked run.
func TestParkStoreConcurrentResolveExactlyOne(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryParkStore()
	_ = s.Put(ctx, parkRec("sess", "o/r#3", "c", 1))

	const racers = 50
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, _ := s.ResolveByPRKey(ctx, "o/r#3"); ok {
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

// TestParkStoreSweep: only records parked before the cutoff are claimed; each exactly once.
func TestParkStoreSweep(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryParkStore()
	old := ParkRecord{SessionID: "old", PRKey: "o/r#1", CallID: "c", Attempts: 1, ParkedAt: time.Now().Add(-time.Hour)}
	fresh := parkRec("new", "o/r#2", "c", 1)
	_ = s.Put(ctx, old)
	_ = s.Put(ctx, fresh)

	swept, err := s.Sweep(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].SessionID != "old" {
		t.Fatalf("sweep = %+v, want only the stale 'old' record", swept)
	}
	if n, _ := s.ParkedCount(ctx); n != 1 {
		t.Errorf("only the fresh run should remain parked, count = %d", n)
	}
	if again, _ := s.Sweep(ctx, time.Now().Add(-time.Minute)); len(again) != 0 {
		t.Errorf("a second sweep should claim nothing more, got %+v", again)
	}
}
