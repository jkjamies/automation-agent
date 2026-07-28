package fixflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"google.golang.org/adk/v2/session"

	"automation-agent/internal/agent/setup"
)

// erroringStore wraps a real ParkStore and forces a chosen method to fail, proving the
// driver surfaces store errors instead of silently dropping a run.
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

func (e *erroringStore) ResolveByPRKey(ctx context.Context, workflow, k string) (setup.ParkRecord, bool, error) {
	if e.failResolve {
		return setup.ParkRecord{}, false, errors.New("resolve boom")
	}
	return e.ParkStore.ResolveByPRKey(ctx, workflow, k)
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

// A store Put failure while recording retry feedback aborts the resume with an error — and
// notifies. A retry only happens after a first attempt already opened a PR, so a run
// dropped here would leave that PR unattended with only a log line to show for it.
func TestResumeRetryPutError(t *testing.T) {
	store := &erroringStore{ParkStore: setup.NewMemoryParkStore()}
	n := &fakeNotifier{}
	e := engineWithStore(t, store, n)
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	store.failPut = true // fail the updateForRetry write on the next (failure) resume
	if err := e.Resume(context.Background(), checkBody("failure", 42, "boom")); err == nil {
		t.Fatal("expected resume to fail when recording retry feedback errors")
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("a retry that cannot be prepared must notify for human review, got %+v", n.msgs)
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
	in := &runParams{owner: "acme", repo: "api", fullRepo: "acme/api", base: "main", report: "r", feedback: "f"}
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

// brokenSessionService fails every write, standing in for the durable session backend
// being unavailable (a Firestore outage). It is the realistic way the drive itself — not
// the attempt inside it — fails.
type brokenSessionService struct{ session.Service }

func (brokenSessionService) Create(context.Context, *session.CreateRequest) (*session.CreateResponse, error) {
	return nil, errors.New("session backend unavailable")
}

// A kickoff whose drive fails must still tell a human. By the time the drive errors the
// attempt may already have pushed a commit and opened a PR, so clearing the run and
// returning only an error leaves that PR with nobody watching it — the dispatcher just
// logs. This is the same guarantee failApply gives an attempt that fails outright.
func TestKickoffDriveFailureNotifies(t *testing.T) {
	n := &fakeNotifier{}
	e := NewEngine(testSpec(), Deps{
		GH: &fakeGH{}, Notify: n, MaxIter: 3, CITimeout: time.Hour,
		SessionService: brokenSessionService{Service: session.InMemoryService()},
		CloneURL:       func(_, _ string) string { return seedRemote(t) },
	})

	err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`))
	if err == nil {
		t.Fatal("expected the kickoff to fail when the session backend is down")
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Fatalf("a failed drive must notify for human review, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("the failed run should be freed, got %d parked", e.driver.parkedCount())
	}
}

// A redelivered kickoff must not break the run. The execution transport retries any 5xx,
// so a second kickoff can arrive for a repo whose agent branch a previous attempt already
// pushed. Recreating that branch from the base and force-pushing nothing would be a
// non-fast-forward rejection that every retry repeats identically — so the apply continues
// the branch that exists instead of trusting the caller's "this is a fresh run".
func TestRedeliveredKickoffContinuesExistingBranch(t *testing.T) {
	remote := seedRemote(t)
	spec := testSpec()
	attempt := 0
	spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		attempt++
		return []FileEdit{{Path: "a.go", Content: fmt.Sprintf("package a\n// attempt %d\n", attempt)}}, nil
	}
	gh := &fakeGH{}
	e := NewEngine(spec, Deps{
		GH: gh, Notify: &fakeNotifier{}, MaxIter: 3, CITimeout: time.Hour,
		CloneURL: func(_, _ string) string { return remote },
	})
	body := []byte(`{"repo":"acme/api","base":"master","report":"r"}`)

	if err := e.Kickoff(context.Background(), body); err != nil {
		t.Fatalf("first kickoff: %v", err)
	}
	// The identical delivery again — a fresh run against a branch that now exists.
	if err := e.Kickoff(context.Background(), body); err != nil {
		t.Fatalf("redelivered kickoff failed (a retry can never succeed if this errors): %v", err)
	}
	if attempt != 2 {
		t.Fatalf("analyze ran %d times, want 2", attempt)
	}
	// The second run's content must be on the branch: proof it continued rather than
	// recreating from base, which would have been rejected at push.
	if !branchHasFile(t, remote, "agent/fix", "a.go") {
		t.Fatal("the agent branch is missing the applied file")
	}
	rr, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	ref, err := rr.Reference(plumbing.NewBranchReferenceName("agent/fix"), true)
	if err != nil {
		t.Fatalf("resolve agent branch: %v", err)
	}
	c, err := rr.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	f, err := c.File("a.go")
	if err != nil {
		t.Fatalf("file on branch tip: %v", err)
	}
	content, _ := f.Contents()
	if !strings.Contains(content, "attempt 2") {
		t.Errorf("branch tip content = %q, want the second attempt's — the redelivery did not land", content)
	}
}

// failingSessionDelete lets a session delete fail while everything else works, which is the
// case the clear ordering exists for.
type failingSessionDelete struct {
	session.Service
	fail bool
}

func (f *failingSessionDelete) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if f.fail {
		return errors.New("session delete unavailable")
	}
	return f.Service.Delete(ctx, req)
}

// clear deletes the session first and keeps the park record if that fails. The record is the
// only thing that can lead anyone back to the session, so deleting it first and then failing
// would leave a session nothing references and no sweep could ever find. Keeping it turns a
// failed cleanup into one the orphan sweep retries.
func TestClearKeepsRecordWhenSessionDeleteFails(t *testing.T) {
	store := setup.NewMemoryParkStore()
	sess := &failingSessionDelete{Service: session.InMemoryService()}
	n := &fakeNotifier{}
	e := NewEngine(testSpec(), Deps{
		GH: &fakeGH{}, Notify: n, MaxIter: 3, CITimeout: time.Hour, ParkStore: store,
		OrphanTTL: time.Hour, SessionService: sess,
		CloneURL: func(_, _ string) string { return seedRemote(t) },
	})
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	sess.fail = true
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// The success is still reported — cleanup trouble must not cost the human their result.
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "succeeded") {
		t.Fatalf("expected the success notification regardless, got %+v", n.msgs)
	}
	// And the record survives as the marker for the orphan sweep to retry.
	orphans, err := store.SweepOrphans(context.Background(), e.spec.Name, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("the park record should survive a failed session delete so cleanup can retry, got %+v", orphans)
	}

	// Once the record has aged past the TTL and the session backend has recovered, the
	// sweep completes the cleanup it could not finish.
	stale := orphans[0]
	stale.UpdatedAt = time.Now().Add(-2 * time.Hour)
	if err := store.Put(context.Background(), stale); err != nil {
		t.Fatalf("age the record: %v", err)
	}
	sess.fail = false
	if err := e.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if left, _ := store.SweepOrphans(context.Background(), e.spec.Name, time.Now().Add(time.Minute)); len(left) != 0 {
		t.Errorf("the orphan should be gone once the session delete succeeds, got %+v", left)
	}
}

// The orphan sweep reaps runs nothing can resolve, and says nothing while doing it: an
// orphan is an already-dead run, not a human waiting on a PR.
func TestSweepOrphansReapsSilently(t *testing.T) {
	store := setup.NewMemoryParkStore()
	n := &fakeNotifier{}
	e := NewEngine(testSpec(), Deps{
		GH: &fakeGH{}, Notify: n, MaxIter: 3, CITimeout: time.Hour, ParkStore: store,
		OrphanTTL: time.Hour, CloneURL: func(_, _ string) string { return seedRemote(t) },
	})
	ctx := context.Background()
	// Created but never parked, and older than the TTL — an instance reclaimed mid-apply.
	_ = store.Put(ctx, setup.ParkRecord{SessionID: "dead", Workflow: e.spec.Name, Params: "{}",
		UpdatedAt: time.Now().Add(-2 * time.Hour)})
	// A run that started moments ago and may still be applying.
	_ = store.Put(ctx, setup.ParkRecord{SessionID: "live", Workflow: e.spec.Name, Params: "{}",
		UpdatedAt: time.Now()})

	if err := e.SweepOrphans(ctx); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if _, ok, _ := store.Get(ctx, "dead"); ok {
		t.Error("the stale unparked run should have been reaped")
	}
	if _, ok, _ := store.Get(ctx, "live"); !ok {
		t.Error("a run that may still be applying must not be reaped")
	}
	if len(n.msgs) != 0 {
		t.Errorf("orphan cleanup must not notify — nobody is waiting on a dead run, got %+v", n.msgs)
	}
}

// A parked run awaiting CI is never treated as an orphan, however long it waits: the
// timeout sweep owns it, and reaping it here would delete a run a human is waiting on.
func TestSweepOrphansLeavesParkedRuns(t *testing.T) {
	store := setup.NewMemoryParkStore()
	n := &fakeNotifier{}
	e := NewEngine(testSpec(), Deps{
		GH: &fakeGH{}, Notify: n, MaxIter: 3, CITimeout: time.Hour, ParkStore: store,
		OrphanTTL: time.Minute, CloneURL: func(_, _ string) string { return seedRemote(t) },
	})
	ctx := context.Background()
	_ = store.Put(ctx, setup.ParkRecord{SessionID: "waiting", Workflow: e.spec.Name,
		PRKey: "acme/api#42", CallID: "c", Attempts: 1, Params: "{}",
		UpdatedAt: time.Now().Add(-2 * time.Hour)})

	if err := e.SweepOrphans(ctx); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if _, ok, _ := store.Get(ctx, "waiting"); !ok {
		t.Fatal("a parked run must survive the orphan sweep")
	}
	if n, _ := store.ParkedCount(ctx, e.spec.Name); n != 1 {
		t.Errorf("parked count = %d, want 1 (still resolvable by a CI webhook)", n)
	}
}
