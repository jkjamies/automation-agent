package setup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"automation-agent/internal/config"
)

var fsPrefixSeq atomic.Int64

// firestorePrefix returns a collection prefix unique to this process and call, so the
// shared Firestore emulator (whose data persists across test runs) cannot leak state
// between cases or between repeated runs.
func firestorePrefix(base string) string {
	return fmt.Sprintf("%s_%d_%d", base, time.Now().UnixNano(), fsPrefixSeq.Add(1))
}

// wf is the workflow every single-engine case in the suite runs under. The store is shared
// by all fix engines, so every claim is workflow-scoped; CrossWorkflowIsolation below is the
// case that exercises two engines at once.
const wf = "lint"

func parkRec(sid, prKey, callID string, attempts int) ParkRecord {
	return ParkRecord{SessionID: sid, Workflow: wf, PRKey: prKey, CallID: callID, Attempts: attempts, UpdatedAt: time.Now()}
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
	if c, ok := s.(io.Closer); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	return s
}

// TestParkStoreConformance runs one behavior suite against every ParkStore implementation,
// so the memory and sqlite backends are guaranteed to behave identically.
func TestParkStoreConformance(t *testing.T) {
	backends := map[string]func(t *testing.T) ParkStore{
		"memory": func(*testing.T) ParkStore { return NewMemoryParkStore() },
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
		if n, _ := s.ParkedCount(ctx, wf); n != 1 {
			t.Fatalf("parked count = %d, want 1", n)
		}
		run, ok, err := s.ResolveByPRKey(ctx, wf, "o/r#1")
		if err != nil || !ok || run.CallID != "c" {
			t.Fatalf("first resolve = %+v, ok=%v, err=%v", run, ok, err)
		}
		if _, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#1"); ok {
			t.Error("second resolve should find nothing (already claimed)")
		}
		if n, _ := s.ParkedCount(ctx, wf); n != 0 {
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
		if _, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#1"); ok {
			t.Error("the stale PR key should no longer resolve")
		}
		run, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#2")
		if !ok || run.Attempts != 2 {
			t.Fatalf("resolve on the current key = %+v, %v (want attempts 2)", run, ok)
		}
		if n, _ := s.ParkedCount(ctx, wf); n != 0 {
			t.Errorf("parked count = %d, want 0 (no stale entry left)", n)
		}
	})

	// Resolving an unparked PR no-ops.
	t.Run("LateResolveNoop", func(t *testing.T) {
		s := newStore(t)
		if _, ok, _ := s.ResolveByPRKey(ctx, wf, "never/parked#9"); ok {
			t.Error("resolving an unparked PR should no-op")
		}
	})

	// An empty key must never match an unparked record (pr_key == "").
	t.Run("EmptyKeyNoResolve", func(t *testing.T) {
		s := newStore(t)
		if err := s.Put(ctx, ParkRecord{SessionID: "sess", Workflow: wf, Params: "x"}); err != nil { // not parked
			t.Fatalf("Put: %v", err)
		}
		if _, ok, _ := s.ResolveByPRKey(ctx, wf, ""); ok {
			t.Error("an empty PR key must not resolve an unparked record")
		}
	})

	// A retry re-parks under the same PR key; the latest CallID/Attempts win.
	t.Run("Repark", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, parkRec("sess", "o/r#4", "c1", 1))
		_ = s.Put(ctx, parkRec("sess", "o/r#4", "c2", 2))
		if n, _ := s.ParkedCount(ctx, wf); n != 1 {
			t.Fatalf("re-park should replace, count = %d", n)
		}
		run, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#4")
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
				if _, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#3"); ok {
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
		stale := ParkRecord{SessionID: "old", Workflow: wf, PRKey: "o/r#1", CallID: "c", Attempts: 1, UpdatedAt: time.Now().Add(-time.Hour)}
		_ = s.Put(ctx, stale)
		_ = s.Put(ctx, parkRec("new", "o/r#2", "c", 1))

		swept, err := s.Sweep(ctx, wf, time.Now().Add(-time.Minute))
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
		if n, _ := s.ParkedCount(ctx, wf); n != 1 {
			t.Errorf("only the fresh run should remain parked, count = %d", n)
		}
		if again, _ := s.Sweep(ctx, wf, time.Now().Add(-time.Minute)); len(again) != 0 {
			t.Errorf("a second sweep should claim nothing more, got %+v", again)
		}
	})

	// A run re-parked with a fresh UpdatedAt after going stale is not swept: re-park updates
	// the cutoff field, so the sweep leaves the fresh attempt alone.
	t.Run("SweepSkipsFreshRepark", func(t *testing.T) {
		s := newStore(t)
		stale := ParkRecord{SessionID: "sess", Workflow: wf, PRKey: "o/r#8", CallID: "c1", Attempts: 1, UpdatedAt: time.Now().Add(-time.Hour)}
		_ = s.Put(ctx, stale)
		// Resolve (a webhook) then re-park (a retry) under the same key, now fresh.
		if _, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#8"); !ok {
			t.Fatal("expected to resolve the stale park")
		}
		_ = s.Put(ctx, parkRec("sess", "o/r#8", "c2", 2)) // UpdatedAt = now

		if swept, err := s.Sweep(ctx, wf, time.Now().Add(-time.Minute)); err != nil || len(swept) != 0 {
			t.Fatalf("sweep = %+v, err=%v; want nothing (the re-park is fresh)", swept, err)
		}
		if run, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#8"); !ok || run.CallID != "c2" {
			t.Errorf("the fresh re-park should still resolve = %+v, ok=%v", run, ok)
		}
	})

	// Two sessions parking under one PR key keep a single active owner (the latest), and
	// deleting the displaced session does not strand the active one.
	t.Run("SingleOwnerPerPRKey", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(ctx, parkRec("A", "o/r#9", "ca", 1))
		_ = s.Put(ctx, parkRec("B", "o/r#9", "cb", 1))
		if n, _ := s.ParkedCount(ctx, wf); n != 1 {
			t.Fatalf("one PR key must have a single active owner, count = %d", n)
		}
		// Deleting the displaced first session must not drop the active owner's index.
		if err := s.Delete(ctx, "A"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		run, ok, _ := s.ResolveByPRKey(ctx, wf, "o/r#9")
		if !ok || run.SessionID != "B" || run.CallID != "cb" {
			t.Fatalf("resolve after displacing A = %+v, ok=%v; want active session B", run, ok)
		}
	})

	// A record that is not parked and has gone stale is an orphan: nothing can ever resolve
	// it. It gets there two ways — created but never parked (the instance died mid-apply), or
	// parked and then displaced when another session took its PR key (a redelivered kickoff).
	// The timeout sweep only looks at parked records, so without this these sit forever
	// holding the whole kickoff report.
	t.Run("SweepOrphans", func(t *testing.T) {
		s := newStore(t)
		// Never parked, stale.
		_ = s.Put(ctx, ParkRecord{SessionID: "never-parked", Workflow: wf, Params: "p",
			UpdatedAt: time.Now().Add(-time.Hour)})
		// Never parked, but recent — a run that may still be applying right now.
		_ = s.Put(ctx, ParkRecord{SessionID: "in-flight", Workflow: wf, Params: "p",
			UpdatedAt: time.Now()})
		// Parked and stale: the timeout sweep owns this one, not the orphan sweep.
		_ = s.Put(ctx, ParkRecord{SessionID: "parked", Workflow: wf, PRKey: "o/r#1", CallID: "c",
			UpdatedAt: time.Now().Add(-time.Hour)})
		// Another workflow's orphan must be left alone.
		_ = s.Put(ctx, ParkRecord{SessionID: "other-wf", Workflow: "coverage", Params: "p",
			UpdatedAt: time.Now().Add(-time.Hour)})

		got, err := s.SweepOrphans(ctx, wf, time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("SweepOrphans: %v", err)
		}
		if len(got) != 1 || got[0].SessionID != "never-parked" {
			t.Fatalf("orphans = %+v, want only never-parked", ids(got))
		}
		// It does not claim, so the record is still there for the caller to delete — and a
		// second sweep sees it again (deleting is idempotent, and this never notifies).
		if again, _ := s.SweepOrphans(ctx, wf, time.Now().Add(-time.Minute)); len(again) != 1 {
			t.Errorf("a second sweep should still see the unclaimed orphan, got %+v", ids(again))
		}
		if err := s.Delete(ctx, "never-parked"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if after, _ := s.SweepOrphans(ctx, wf, time.Now().Add(-time.Minute)); len(after) != 0 {
			t.Errorf("nothing should remain after deleting the orphan, got %+v", ids(after))
		}
		// The parked run is untouched: still parked, still resolvable.
		if n, _ := s.ParkedCount(ctx, wf); n != 1 {
			t.Errorf("parked count = %d, want 1 (the orphan sweep must not unpark anything)", n)
		}
	})

	// Every fix engine shares ONE store (one Firestore instance and collection), so a claim
	// must never cross workflows. Without this scoping, whichever engine swept first resolved
	// every stale run — reporting it under the wrong workflow's title and awaited check name,
	// and deleting the ADK session under the wrong app name so it leaked instead.
	t.Run("CrossWorkflowIsolation", func(t *testing.T) {
		s := newStore(t)
		// Two engines parked on the same repo. The PR numbers differ in practice (each engine
		// pushes its own branch), but they are made identical here to pin the strongest case.
		lint := ParkRecord{SessionID: "lint-sess", Workflow: "lint", PRKey: "o/r#5", CallID: "cl",
			Attempts: 1, UpdatedAt: time.Now().Add(-time.Hour)}
		cov := ParkRecord{SessionID: "cov-sess", Workflow: "coverage", PRKey: "o/r#5", CallID: "cc",
			Attempts: 1, UpdatedAt: time.Now().Add(-time.Hour)}
		if err := s.Put(ctx, lint); err != nil {
			t.Fatalf("Put lint: %v", err)
		}
		if err := s.Put(ctx, cov); err != nil {
			t.Fatalf("Put coverage: %v", err)
		}
		// Parking coverage must not displace lint: the single-owner rule is per (workflow, key).
		if n, _ := s.ParkedCount(ctx, "lint"); n != 1 {
			t.Fatalf("lint parked count = %d, want 1 (coverage must not displace it)", n)
		}

		// A resume for lint's check claims only lint's run.
		run, ok, err := s.ResolveByPRKey(ctx, "lint", "o/r#5")
		if err != nil || !ok || run.SessionID != "lint-sess" {
			t.Fatalf("lint resolve = %+v, ok=%v, err=%v; want lint-sess", run, ok, err)
		}
		if n, _ := s.ParkedCount(ctx, "coverage"); n != 1 {
			t.Fatalf("coverage still parked count = %d, want 1 (lint's resolve must not claim it)", n)
		}

		// The lint engine's sweep must find nothing left of its own and never touch coverage's.
		swept, err := s.Sweep(ctx, "lint", time.Now().Add(-time.Minute))
		if err != nil || len(swept) != 0 {
			t.Fatalf("lint sweep = %+v, err=%v; want nothing (already resolved)", swept, err)
		}
		if _, ok, _ := s.Get(ctx, "cov-sess"); !ok {
			t.Fatal("the coverage run must survive the lint engine's sweep")
		}

		// Coverage's own sweep still claims its run — and reports it as coverage's.
		swept, err = s.Sweep(ctx, "coverage", time.Now().Add(-time.Minute))
		if err != nil || len(swept) != 1 || swept[0].SessionID != "cov-sess" {
			t.Fatalf("coverage sweep = %+v, err=%v; want the coverage run", swept, err)
		}
		if swept[0].Workflow != "coverage" {
			t.Errorf("swept record Workflow = %q, want coverage (the summary is framed from it)", swept[0].Workflow)
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
	run, ok, err := s2.ResolveByPRKey(ctx, wf, "o/r#7")
	if err != nil || !ok {
		t.Fatalf("cross-process resolve = ok %v, err %v", ok, err)
	}
	if run.CallID != "call-7" || run.Attempts != 2 {
		t.Errorf("recovered record = %+v, want call-7/2", run)
	}
}

// NewParkStore is the backend switch the entrypoint calls: it must mirror SESSION_BACKEND
// exactly, since the park store and the session service have to agree on durability. The
// firestore arm needs a live client, so it is covered by the emulator-gated suite above.
func TestNewParkStoreSelectsBackend(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "switch.db")

	for _, tc := range []struct {
		backend config.SessionBackend
		want    string
	}{
		{config.SessionMemory, "*setup.memoryParkStore"},
		{config.SessionSQLite, "*setup.sqliteParkStore"},
	} {
		t.Run(string(tc.backend), func(t *testing.T) {
			s, err := NewParkStore(ctx, config.Config{SessionBackend: tc.backend, SQLiteDSN: dsn})
			if err != nil {
				t.Fatalf("NewParkStore(%s): %v", tc.backend, err)
			}
			if got := fmt.Sprintf("%T", s); got != tc.want {
				t.Errorf("backend %s built %s, want %s", tc.backend, got, tc.want)
			}
		})
	}

	// An unrecognized backend is an error, not a silent fall back to memory: falling back
	// would hand a misconfigured deployment a store that strands every parked run on restart.
	if _, err := NewParkStore(ctx, config.Config{SessionBackend: "nope"}); err == nil {
		t.Error("expected an error for an unknown session backend")
	}
}

// The store a run parks in and the session service that holds its paused history must be
// backed by the same durability tier — a durable park record pointing at an in-memory
// session cannot resume after a restart. Both switches read SESSION_BACKEND, so this pins
// that they agree on every value.
func TestParkStoreAndSessionServiceAgreeOnBackend(t *testing.T) {
	ctx := context.Background()
	for _, backend := range []config.SessionBackend{config.SessionMemory, config.SessionSQLite} {
		cfg := config.Config{SessionBackend: backend, SQLiteDSN: "file:" + filepath.Join(t.TempDir(), "pair.db")}
		if _, err := NewParkStore(ctx, cfg); err != nil {
			t.Errorf("park store for %s: %v", backend, err)
		}
		if _, err := NewSessionService(ctx, cfg); err != nil {
			t.Errorf("session service for %s: %v", backend, err)
		}
	}
	// And both reject the same unknown value rather than one of them defaulting.
	bad := config.Config{SessionBackend: "nope"}
	_, parkErr := NewParkStore(ctx, bad)
	_, sessErr := NewSessionService(ctx, bad)
	if parkErr == nil || sessErr == nil {
		t.Errorf("both switches must reject an unknown backend (park=%v, session=%v)", parkErr, sessErr)
	}
}

// ids renders session ids for readable sweep failures.
func ids(recs []ParkRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.SessionID)
	}
	return out
}
