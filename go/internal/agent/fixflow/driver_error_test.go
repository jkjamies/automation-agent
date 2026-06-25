package fixflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"automation-agent/internal/agent/setup"
)

// erroringStore wraps a real ParkStore and forces a chosen method to fail, proving the
// Driver surfaces store errors instead of silently dropping a run.
type erroringStore struct {
	setup.ParkStore
	failGet, failPut, failResolve, failDelete bool
}

func (e *erroringStore) Delete(ctx context.Context, sid string) error {
	if e.failDelete {
		return errors.New("delete boom")
	}
	return e.ParkStore.Delete(ctx, sid)
}

func (e *erroringStore) Get(ctx context.Context, sid string) (setup.ParkRecord, bool, error) {
	if e.failGet {
		return setup.ParkRecord{}, false, errors.New("get boom")
	}
	return e.ParkStore.Get(ctx, sid)
}

func (e *erroringStore) Put(ctx context.Context, r setup.ParkRecord) error {
	if e.failPut {
		return errors.New("put boom")
	}
	return e.ParkStore.Put(ctx, r)
}

func (e *erroringStore) ResolveByPRKey(ctx context.Context, k string) (setup.ParkRecord, bool, error) {
	if e.failResolve {
		return setup.ParkRecord{}, false, errors.New("resolve boom")
	}
	return e.ParkStore.ResolveByPRKey(ctx, k)
}

func engineWithStore(t *testing.T, store setup.ParkStore, n *fakeNotifier) *Engine {
	return NewEngine(testSpec(), Deps{
		GH: &fakeGH{}, Notify: n, MaxIter: 3, CITimeout: time.Hour, ParkStore: store,
		CloneURL: func(_, _ string) string { return seedRemote(t) },
	})
}

// A store Put failure at kickoff aborts the run with an error rather than proceeding.
func TestKickoffPutError(t *testing.T) {
	e := engineWithStore(t, &erroringStore{ParkStore: setup.NewMemoryParkStore(), failPut: true}, &fakeNotifier{})
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err == nil {
		t.Fatal("expected kickoff to fail when the store Put errors")
	}
}

// A store Get failure inside apply_fix surfaces as an apply failure (notifies + errors),
// not a silently dropped run.
func TestApplyFixGetError(t *testing.T) {
	n := &fakeNotifier{}
	e := engineWithStore(t, &erroringStore{ParkStore: setup.NewMemoryParkStore(), failGet: true}, n)
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err == nil {
		t.Fatal("expected kickoff to fail when apply_fix cannot load run params")
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected a needs-review notification, got %+v", n.msgs)
	}
}

// A store ResolveByPRKey failure on resume returns an error.
func TestResumeResolveError(t *testing.T) {
	store := &erroringStore{ParkStore: setup.NewMemoryParkStore()}
	e := engineWithStore(t, store, &fakeNotifier{})
	seedParked(e, "acme/api#42", "run-x", "c", 1)
	store.failResolve = true
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err == nil {
		t.Fatal("expected resume to fail when the store resolve errors")
	}
}

// A store Put failure while recording retry feedback aborts the resume with an error.
func TestResumeRetryPutError(t *testing.T) {
	store := &erroringStore{ParkStore: setup.NewMemoryParkStore()}
	e := engineWithStore(t, store, &fakeNotifier{})
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	store.failPut = true // fail the updateForRetry write on the next (failure) resume
	if err := e.Resume(context.Background(), checkBody("failure", 42, "boom")); err == nil {
		t.Fatal("expected resume to fail when recording retry feedback errors")
	}
}

// onTimeout for a run that is already gone is a benign no-op (no notification).
func TestOnTimeoutAlreadyResolved(t *testing.T) {
	n := &fakeNotifier{}
	e := engineWithStore(t, setup.NewMemoryParkStore(), n)
	e.driver.onTimeout("acme/api#999")
	if len(n.msgs) != 0 {
		t.Errorf("timeout on an unparked PR should not notify, got %+v", n.msgs)
	}
}

// A Delete failure during terminal cleanup is logged but does not block the success path.
func TestClearDeleteErrorStillNotifies(t *testing.T) {
	store := &erroringStore{ParkStore: setup.NewMemoryParkStore()}
	n := &fakeNotifier{}
	e := engineWithStore(t, store, n)
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	store.failDelete = true
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "succeeded") {
		t.Errorf("success should still notify despite a delete error, got %+v", n.msgs)
	}
}

// marshalRunParams/unmarshalRunParams round-trip, and a malformed blob is rejected.
func TestRunParamsRoundTrip(t *testing.T) {
	in := &runParams{owner: "acme", repo: "api", fullRepo: "acme/api", base: "main", report: "r", feedback: "f", newBranch: true}
	blob, err := marshalRunParams(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := unmarshalRunParams(blob)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if *out != *in {
		t.Errorf("round-trip = %+v, want %+v", *out, *in)
	}
	if _, err := unmarshalRunParams("{not json"); err == nil {
		t.Error("expected an error decoding a malformed params blob")
	}
}
