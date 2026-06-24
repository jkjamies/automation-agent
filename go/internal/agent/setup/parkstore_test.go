package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fsPrefixSeq atomic.Int64

// firestorePrefix returns a collection prefix unique to this process and call, so the
// shared Firestore emulator (whose data persists across test runs) cannot leak state
// between cases or between repeated runs.
func firestorePrefix(base string) string {
	return fmt.Sprintf("%s_%d_%d", base, time.Now().UnixNano(), fsPrefixSeq.Add(1))
}

func parkRec(sid, prKey, callID string, attempts int) ParkRecord {
	return ParkRecord{SessionID: sid, PRKey: prKey, CallID: callID, Attempts: attempts, ParkedAt: time.Now()}
}

func newSQLiteParkStore(t *testing.T) ParkStore {
	t.Helper()
	s, err := NewSQLiteParkStore("file:" + filepath.Join(t.TempDir(), "park.db"))
	if err != nil {
		t.Fatalf("new sqlite park store: %v", err)
	}
	return s
}

// newFirestoreParkStore builds a store against the Firestore emulator (FIRESTORE_EMULATOR_HOST).
// Each call uses a collection unique to the running subtest, so the shared emulator state
// does not leak between cases.
func newFirestoreParkStore(t *testing.T) ParkStore {
	t.Helper()
	ctx := context.Background()
	s, err := NewFirestoreParkStore(ctx, "test-project", firestorePrefix("park"))
	if err != nil {
		t.Fatalf("new firestore park store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestParkStoreConformance runs one behavior suite against every ParkStore implementation,
// so the memory and sqlite backends are guaranteed to behave identically.
func TestParkStoreConformance(t *testing.T) {
	backends := map[string]func(t *testing.T) ParkStore{
		"memory": func(t *testing.T) ParkStore { return NewMemoryParkStore() },
		"sqlite": newSQLiteParkStore,
	}
	// The firestore backend joins the suite only when the emulator is reachable, so CI
	// without it still runs memory + sqlite.
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" {
		backends["firestore"] = newFirestoreParkStore
	}
	for name, newStore := range backends {
		t.Run(name, func(t *testing.T) { runParkStoreSuite(t, newStore) })
	}
}

func runParkStoreSuite(t *testing.T, newStore func(t *testing.T) ParkStore) {
	ctx := context.Background()

	// A parked record resolves exactly once; the per-run record survives for a retry until
	// Delete.
	t.Run("ResolveOnce", func(t *testing.T) {
		s := newStore(t)
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
		if _, ok, _ := s.Get(ctx, "sess"); !ok {
			t.Error("per-run record should survive a resolve")
		}
		if err := s.Delete(ctx, "sess"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, ok, _ := s.Get(ctx, "sess"); ok {
			t.Error("record should be gone after Delete")
		}
	})

	// Re-Putting a session under a different PR key drops the old key's index.
	t.Run("PutClearsStaleIndex", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, parkRec("sess", "o/r#1", "c", 1))
		_ = s.Put(ctx, parkRec("sess", "o/r#2", "c", 2))
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
	})

	// Resolving an unparked PR no-ops.
	t.Run("LateResolveNoop", func(t *testing.T) {
		s := newStore(t)
		if _, ok, _ := s.ResolveByPRKey(ctx, "never/parked#9"); ok {
			t.Error("resolving an unparked PR should no-op")
		}
	})

	// An empty key must never match an unparked record (pr_key == "").
	t.Run("EmptyKeyNoResolve", func(t *testing.T) {
		s := newStore(t)
		if err := s.Put(ctx, ParkRecord{SessionID: "sess", Params: "x"}); err != nil { // not parked
			t.Fatalf("Put: %v", err)
		}
		if _, ok, _ := s.ResolveByPRKey(ctx, ""); ok {
			t.Error("an empty PR key must not resolve an unparked record")
		}
	})

	// A retry re-parks under the same PR key; the latest CallID/Attempts win.
	t.Run("Repark", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, parkRec("sess", "o/r#4", "c1", 1))
		_ = s.Put(ctx, parkRec("sess", "o/r#4", "c2", 2))
		if n, _ := s.ParkedCount(ctx); n != 1 {
			t.Fatalf("re-park should replace, count = %d", n)
		}
		run, ok, _ := s.ResolveByPRKey(ctx, "o/r#4")
		if !ok || run.CallID != "c2" || run.Attempts != 2 {
			t.Fatalf("resolve = %+v, %v (want latest c2/2)", run, ok)
		}
	})

	// Under contention exactly one caller claims a parked run.
	t.Run("ConcurrentResolveExactlyOne", func(t *testing.T) {
		s := newStore(t)
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
	})

	// Only records parked before the cutoff are claimed; each exactly once.
	t.Run("Sweep", func(t *testing.T) {
		s := newStore(t)
		stale := ParkRecord{SessionID: "old", PRKey: "o/r#1", CallID: "c", Attempts: 1, ParkedAt: time.Now().Add(-time.Hour)}
		_ = s.Put(ctx, stale)
		_ = s.Put(ctx, parkRec("new", "o/r#2", "c", 1))

		swept, err := s.Sweep(ctx, time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if len(swept) != 1 || swept[0].SessionID != "old" {
			t.Fatalf("sweep = %+v, want only the stale 'old' record", swept)
		}
		// The swept record must keep its PRKey: the driver needs it to stop the run's timer
		// and name the PR in the timeout summary.
		if swept[0].PRKey != "o/r#1" {
			t.Errorf("swept record PRKey = %q, want o/r#1 (retained for timeout cleanup)", swept[0].PRKey)
		}
		if n, _ := s.ParkedCount(ctx); n != 1 {
			t.Errorf("only the fresh run should remain parked, count = %d", n)
		}
		if again, _ := s.Sweep(ctx, time.Now().Add(-time.Minute)); len(again) != 0 {
			t.Errorf("a second sweep should claim nothing more, got %+v", again)
		}
	})

	// A run re-parked with a fresh ParkedAt after going stale is not swept: re-park updates
	// the cutoff field, so the sweep leaves the fresh attempt alone.
	t.Run("SweepSkipsFreshRepark", func(t *testing.T) {
		s := newStore(t)
		stale := ParkRecord{SessionID: "sess", PRKey: "o/r#8", CallID: "c1", Attempts: 1, ParkedAt: time.Now().Add(-time.Hour)}
		_ = s.Put(ctx, stale)
		// Resolve (a webhook) then re-park (a retry) under the same key, now fresh.
		if _, ok, _ := s.ResolveByPRKey(ctx, "o/r#8"); !ok {
			t.Fatal("expected to resolve the stale park")
		}
		_ = s.Put(ctx, parkRec("sess", "o/r#8", "c2", 2)) // ParkedAt = now

		if swept, err := s.Sweep(ctx, time.Now().Add(-time.Minute)); err != nil || len(swept) != 0 {
			t.Fatalf("sweep = %+v, err=%v; want nothing (the re-park is fresh)", swept, err)
		}
		if run, ok, _ := s.ResolveByPRKey(ctx, "o/r#8"); !ok || run.CallID != "c2" {
			t.Errorf("the fresh re-park should still resolve = %+v, ok=%v", run, ok)
		}
	})

	// Two sessions parking under one PR key keep a single active owner (the latest), and
	// deleting the displaced session does not strand the active one.
	t.Run("SingleOwnerPerPRKey", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, parkRec("A", "o/r#9", "ca", 1))
		_ = s.Put(ctx, parkRec("B", "o/r#9", "cb", 1))
		if n, _ := s.ParkedCount(ctx); n != 1 {
			t.Fatalf("one PR key must have a single active owner, count = %d", n)
		}
		// Deleting the displaced first session must not drop the active owner's index.
		if err := s.Delete(ctx, "A"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		run, ok, _ := s.ResolveByPRKey(ctx, "o/r#9")
		if !ok || run.SessionID != "B" || run.CallID != "cb" {
			t.Fatalf("resolve after displacing A = %+v, ok=%v; want active session B", run, ok)
		}
	})
}

// TestSQLiteParkStoreCrossProcess proves park records survive a restart: a record written
// through one store is resolvable through a fresh store over the same file.
func TestSQLiteParkStoreCrossProcess(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "park.db")

	s1, err := NewSQLiteParkStore(dsn)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	if err := s1.Put(ctx, parkRec("sess", "o/r#7", "call-7", 2)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A brand-new store over the same file (simulating a restart) still sees the parked run.
	s2, err := NewSQLiteParkStore(dsn)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	run, ok, err := s2.ResolveByPRKey(ctx, "o/r#7")
	if err != nil || !ok {
		t.Fatalf("cross-process resolve = ok %v, err %v", ok, err)
	}
	if run.CallID != "call-7" || run.Attempts != 2 {
		t.Errorf("recovered record = %+v, want call-7/2", run)
	}
}
