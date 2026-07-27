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
