package fixflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

// seedParked puts a parked run directly into the driver's store, for tests that exercise
// Resume/timeout without driving a real kickoff first. The record is stamped with the
// engine's workflow, since every claim is workflow-scoped (the store is shared).
func seedParked(e *Engine, prKey, sid, callID string, attempts int) {
	_ = e.driver.store.Put(context.Background(), setup.ParkRecord{
		SessionID: sid, Workflow: e.spec.Name, PRKey: prKey, CallID: callID,
		Attempts: attempts, UpdatedAt: time.Now(),
	})
}

// testSpec is a Spec with deterministic fake triage/analyze (no LLM), so the engine
// loop can be tested in isolation.
func testSpec() Spec {
	return Spec{
		Name: "test", Branch: "agent/fix", CheckName: "agent-test-verify",
		CommitMessage: "fix", PRTitle: "Fix",
		SuccessTitle: "Fix succeeded", ReviewTitle: "Needs human review", CleanTitle: "Already clean",
		Triage: func(_ context.Context, _ model.LLM, _ string) ([]FileWork, error) {
			return []FileWork{{Path: "a.go", Items: []string{"x"}}}, nil
		},
		Analyze: func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
			return []FileEdit{{Path: "a.go", Content: "package a\n"}}, nil
		},
	}
}

func newEngine(remote string, gh *fakeGH, n *fakeNotifier) *Engine {
	return NewEngine(testSpec(), Deps{
		GH: gh, Notify: n, MaxIter: 3, CITimeout: time.Hour, PRLabel: "automation-agent",
		CloneURL: func(_, _ string) string { return remote },
	})
}

// checkBody builds a check_run webhook payload for the test engine's check.
func checkBody(conclusion string, pr int, output string) []byte {
	return []byte(fmt.Sprintf(
		`{"action":"completed","check_run":{"name":"agent-test-verify","status":"completed","conclusion":%q,"pull_requests":[{"number":%d,"head":{"ref":"agent/fix"}}],"output":{"text":%q}},"repository":{"full_name":"acme/api"}}`,
		conclusion, pr, output))
}

// Kickoff applies a fix (creating the PR) and parks the run awaiting CI.
func TestEngineKickoffParks(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	e := newEngine(remote, gh, &fakeNotifier{})

	raw := []byte(`{"repo":"acme/api","base":"master","report":"r"}`)
	if err := e.Kickoff(context.Background(), raw); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.created == nil || gh.created.Head != "agent/fix" {
		t.Errorf("expected PR on agent/fix, got %+v", gh.created)
	}
	if len(gh.labeled) != 1 {
		t.Errorf("expected label, got %v", gh.labeled)
	}
	if e.driver.parkedCount() != 1 {
		t.Errorf("expected one parked run awaiting CI, got %d", e.driver.parkedCount())
	}
}

// Triage finding nothing actionable finishes as a positive clean outcome: no PR is
// opened, no run is parked, the clean notification (not the review alarm) is sent, and
// Kickoff returns nil so the dispatcher does not log a no-op as a failure.
func TestEngineKickoffClean(t *testing.T) {
	gh := &fakeGH{}
	n := &fakeNotifier{}
	spec := testSpec()
	spec.Triage = func(_ context.Context, _ model.LLM, _ string) ([]FileWork, error) {
		return nil, fmt.Errorf("triage: nothing here: %w", ErrNoWork)
	}
	e := NewEngine(spec, Deps{
		GH: gh, Notify: n, MaxIter: 3, CITimeout: time.Hour, PRLabel: "automation-agent",
		CloneURL: func(_, _ string) string { return seedRemote(t) },
	})

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("clean kickoff should not error: %v", err)
	}
	if gh.created != nil {
		t.Errorf("clean kickoff should not open a PR, got %+v", gh.created)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("clean kickoff should not park a run, got %d", e.driver.parkedCount())
	}
	if len(n.msgs) != 1 || n.msgs[0].Title != "Already clean" {
		t.Fatalf("expected one clean notification titled %q, got %+v", "Already clean", n.msgs)
	}
	if strings.Contains(n.msgs[0].Text, "review") {
		t.Errorf("clean notice must not mention review: %q", n.msgs[0].Text)
	}
}

// A successful CI conclusion resolves the parked run and notifies success.
func TestEngineResumeSuccess(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "succeeded") {
		t.Errorf("expected success notification, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("success should free the parked run, got %d", e.driver.parkedCount())
	}
}

// A CI failure that has exhausted MaxIter asks for human review.
func TestEngineResumeExhausted(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	// Park a run that has already used all attempts.
	seedParked(e, "acme/api#42", "run-x", "c", 3)

	if err := e.Resume(context.Background(), checkBody("failure", 42, "still broken")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected needs-review notification, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("exhausted run should be freed, got %d", e.driver.parkedCount())
	}
}

// A CI failure with attempts remaining re-applies on the same PR and re-parks.
func TestEngineResumeRetry(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	n := &fakeNotifier{}
	e := newEngine(remote, gh, n)

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	// A different analyze result on retry so there is a real change to commit.
	e.spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		return []FileEdit{{Path: "a.go", Content: "package a\n\n// retry\n"}}, nil
	}
	gh.existing = []githubapi.PR{{Number: 42, Branch: "agent/fix"}}
	gh.created = nil

	if err := e.Resume(context.Background(), checkBody("failure", 42, "still failing")); err != nil {
		t.Fatalf("Resume retry: %v", err)
	}
	if gh.created != nil {
		t.Error("retry should reuse the PR, not create a new one")
	}
	if len(n.msgs) != 0 {
		t.Errorf("retry should not notify, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 1 {
		t.Errorf("retry should leave the run parked, got %d", e.driver.parkedCount())
	}
}

// The full loop: kickoff → fail → fail → fail counts attempts in memory and gives up at
// MaxIter, proving tries are counted by the registry (not from GitHub).
func TestEngineFullLoopExhausts(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{existing: []githubapi.PR{{Number: 42, Branch: "agent/fix"}}}
	n := &fakeNotifier{}
	spec := testSpec()
	calls := 0
	spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		calls++ // vary content so every attempt is a real commit
		return []FileEdit{{Path: "a.go", Content: fmt.Sprintf("package a\n// v%d\n", calls)}}, nil
	}
	e := NewEngine(spec, Deps{GH: gh, Notify: n, MaxIter: 3, CITimeout: time.Hour,
		CloneURL: func(_, _ string) string { return remote }})

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	// Two failures are retried (attempts 2, 3); the third gives up.
	for i := 0; i < 2; i++ {
		if err := e.Resume(context.Background(), checkBody("failure", 42, "boom")); err != nil {
			t.Fatalf("Resume #%d: %v", i+1, err)
		}
		if len(n.msgs) != 0 {
			t.Fatalf("attempt %d should not notify yet, got %+v", i+2, n.msgs)
		}
		if e.driver.parkedCount() != 1 {
			t.Fatalf("attempt %d should re-park, got %d", i+2, e.driver.parkedCount())
		}
	}
	if err := e.Resume(context.Background(), checkBody("failure", 42, "boom")); err != nil {
		t.Fatalf("Resume final: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected needs-review after MaxIter, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("run should be freed after giving up, got %d", e.driver.parkedCount())
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 apply attempts, got %d", calls)
	}
}

// When CI never reports, the per-run timeout frees the run and asks for review.
func TestEngineTimeoutFreesRun(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	seedParked(e, "acme/api#42", "run-x", "c", 1)

	e.driver.onTimeout("acme/api#42")
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected timeout review notification, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("timeout should free the run, got %d", e.driver.parkedCount())
	}
	// A late webhook after the timeout is a benign no-op.
	if err := e.Resume(context.Background(), checkBody("success", 42, "")); err != nil {
		t.Fatalf("late resume: %v", err)
	}
	if len(n.msgs) != 1 {
		t.Errorf("late webhook after timeout should not notify again, got %+v", n.msgs)
	}
}

// A run parked longer ago than CITimeout is resolved by the durable sweep (notify + free),
// the catch-all behind the soft in-memory timer.
func TestEngineSweepTimesOutStaleRun(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	_ = e.driver.store.Put(context.Background(), setup.ParkRecord{
		SessionID: "run-x", Workflow: e.spec.Name, PRKey: "acme/api#42", CallID: "c", Attempts: 1,
		UpdatedAt: time.Now().Add(-2 * time.Hour), // older than the 1h CITimeout
	})
	if err := e.SweepTimeouts(context.Background()); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected a timeout review notification, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("swept run should be freed, got %d", e.driver.parkedCount())
	}
}

// A freshly parked run is left alone by the sweep.
func TestEngineSweepSkipsFreshRun(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	seedParked(e, "acme/api#42", "run-x", "c", 1) // UpdatedAt = now
	if err := e.SweepTimeouts(context.Background()); err != nil {
		t.Fatalf("SweepTimeouts: %v", err)
	}
	if len(n.msgs) != 0 {
		t.Errorf("a fresh run must not be swept, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 1 {
		t.Errorf("fresh run should remain parked, got %d", e.driver.parkedCount())
	}
}

// A conclusion for an unknown/already-resolved PR is a no-op.
func TestEngineResumeUnknownPR(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	if err := e.Resume(context.Background(), checkBody("success", 99, "")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 0 {
		t.Errorf("unknown PR should be ignored, got %+v", n.msgs)
	}
}

func TestEngineResumeIgnoresOtherCheck(t *testing.T) {
	n := &fakeNotifier{}
	e := newEngine(seedRemote(t), &fakeGH{}, n)
	body := `{"check_run":{"name":"some-other-check","status":"completed","conclusion":"failure"},"repository":{"full_name":"acme/api"}}`
	if err := e.Resume(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(n.msgs) != 0 {
		t.Error("a non-matching check should be ignored")
	}
}

func TestEngineKickoffTriageError(t *testing.T) {
	spec := testSpec()
	spec.Triage = func(context.Context, model.LLM, string) ([]FileWork, error) {
		return nil, errors.New("triage boom")
	}
	e := NewEngine(spec, Deps{GH: &fakeGH{}, CITimeout: time.Hour, CloneURL: func(_, _ string) string { return seedRemote(t) }})
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","report":"r"}`)); err == nil {
		t.Fatal("expected triage error to propagate")
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("a failed apply should not park a run, got %d", e.driver.parkedCount())
	}
}

// An apply-step failure (here: analyze errors before a PR can be opened) frees the run
// and notifies a human for review — it must not vanish with only a log line.
func TestEngineApplyFailureNotifies(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	n := &fakeNotifier{}
	e := newEngine(remote, gh, n)
	e.spec.Analyze = func(_ context.Context, _ AnalyzeInput) ([]FileEdit, error) {
		return nil, errors.New("analyze boom")
	}

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"master","report":"r"}`)); err == nil {
		t.Fatal("expected apply failure to propagate")
	}
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0].Title, "review") {
		t.Errorf("expected a needs-review notification on apply failure, got %+v", n.msgs)
	}
	if e.driver.parkedCount() != 0 {
		t.Errorf("a failed apply should not leave a parked run, got %d", e.driver.parkedCount())
	}
}

func TestEngineLabelAndCheckName(t *testing.T) {
	e := newEngine("x", &fakeGH{}, &fakeNotifier{})
	if e.Label() != "automation-agent" || e.CheckName() != "agent-test-verify" {
		t.Errorf("label=%q check=%q", e.Label(), e.CheckName())
	}
}

func TestCloneURLByTransport(t *testing.T) {
	// Default (https) and an explicit ssh transport build the two GitHub URL forms; a
	// test-injected CloneURL still overrides both.
	https := (&Engine{d: Deps{}}).cloneURL("acme", "api")
	if want := "https://github.com/acme/api.git"; https != want {
		t.Errorf("https cloneURL = %q, want %q", https, want)
	}
	ssh := (&Engine{d: Deps{GitTransport: "ssh"}}).cloneURL("acme", "api")
	if want := "git@github.com:acme/api.git"; ssh != want {
		t.Errorf("ssh cloneURL = %q, want %q", ssh, want)
	}
	override := (&Engine{d: Deps{GitTransport: "ssh", CloneURL: func(_, _ string) string { return "x" }}}).cloneURL("acme", "api")
	if override != "x" {
		t.Errorf("injected CloneURL override = %q, want x", override)
	}
}

// Production wires every fix engine onto ONE shared ParkStore (a single Firestore instance
// and collection), so an engine's sweep must claim only its own runs. It previously claimed
// all of them: whichever engine swept first resolved every stale run, notified under the
// wrong workflow's title and awaited check name, and deleted the ADK session under the wrong
// app name — so the real session leaked in the durable backend.
func TestSweepDoesNotClaimAnotherEnginesRun(t *testing.T) {
	shared := setup.NewMemoryParkStore()
	remote := seedRemote(t)
	deps := func(n *fakeNotifier) Deps {
		return Deps{
			GH: &fakeGH{}, Notify: n, MaxIter: 3, CITimeout: time.Hour, ParkStore: shared,
			CloneURL: func(_, _ string) string { return remote },
		}
	}
	lintNotes, covNotes := &fakeNotifier{}, &fakeNotifier{}
	lintSpec := testSpec()
	lintSpec.Name, lintSpec.CheckName, lintSpec.ReviewTitle = "lint", "agent-lint-verify", "Lint needs review"
	covSpec := testSpec()
	covSpec.Name, covSpec.CheckName, covSpec.ReviewTitle = "coverage", "agent-coverage-verify", "Coverage needs review"
	lint := NewEngine(lintSpec, deps(lintNotes))
	cov := NewEngine(covSpec, deps(covNotes))

	// Only the coverage engine has a stale parked run.
	_ = shared.Put(context.Background(), setup.ParkRecord{
		SessionID: "cov-sess", Workflow: "coverage", PRKey: "acme/api#42", CallID: "c1", Attempts: 1,
		Params:    `{"owner":"acme","repo":"api","full_repo":"acme/api","base":"main"}`,
		UpdatedAt: time.Now().Add(-2 * time.Hour),
	})

	if err := lint.SweepTimeouts(context.Background()); err != nil {
		t.Fatalf("lint sweep: %v", err)
	}
	if len(lintNotes.msgs) != 0 {
		t.Errorf("the lint engine notified about a coverage run: %+v", lintNotes.msgs)
	}
	if _, ok, _ := shared.Get(context.Background(), "cov-sess"); !ok {
		t.Fatal("the lint engine's sweep deleted the coverage engine's run")
	}

	// The owning engine still resolves it, framed as coverage.
	if err := cov.SweepTimeouts(context.Background()); err != nil {
		t.Fatalf("coverage sweep: %v", err)
	}
	if len(covNotes.msgs) != 1 {
		t.Fatalf("coverage notifications = %d, want 1", len(covNotes.msgs))
	}
	if got := covNotes.msgs[0].Title; got != "Coverage needs review" {
		t.Errorf("timeout notice title = %q, want the coverage engine's", got)
	}
	if _, ok, _ := shared.Get(context.Background(), "cov-sess"); ok {
		t.Error("the coverage engine's sweep should clear its own run")
	}
}

// A kickoff that omits "base" resolves the repository's real default branch and uses it for
// the branch point and the PR base. It previously defaulted to the literal "main", so on any
// repo whose default is master/develop/trunk the fix was cut from one ref while its PR
// targeted another that may not even exist.
func TestKickoffResolvesDefaultBranch(t *testing.T) {
	gh := &fakeGH{defaultBranch: "master"} // what git.PlainInit gives the seeded remote
	e := newEngine(seedRemote(t), gh, &fakeNotifier{})

	// No "base" in the payload.
	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.created == nil {
		t.Fatal("expected a PR to be created")
	}
	if gh.created.Base != "master" {
		t.Errorf("PR base = %q, want the repo's real default branch (master), not a hardcoded name", gh.created.Base)
	}
}

// An explicit base in the payload wins over the repo default, and the fix branch is actually
// cut from it — the clone must check that ref out rather than landing on the remote's default.
func TestKickoffHonorsExplicitBase(t *testing.T) {
	remote := seedRemoteWithBranch(t, "develop")
	gh := &fakeGH{defaultBranch: "master"}
	e := newEngine(remote, gh, &fakeNotifier{})

	if err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","base":"develop","report":"r"}`)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.created == nil || gh.created.Base != "develop" {
		t.Fatalf("PR = %+v, want base develop", gh.created)
	}
	// The pushed branch must descend from develop, not from the remote's default branch.
	// develop carries a file master does not, so its presence proves the branch point.
	if !branchHasFile(t, remote, "agent/fix", "only-on-develop.txt") {
		t.Error("the fix branch was not cut from the explicit base (develop)")
	}
}

// When the base cannot be resolved, the kickoff fails loudly instead of guessing a branch
// name and surfacing an opaque 422 later when the PR is opened.
func TestKickoffFailsWhenDefaultBranchUnresolvable(t *testing.T) {
	gh := &fakeGH{defaultBranchErr: errors.New("api down")}
	e := newEngine(seedRemote(t), gh, &fakeNotifier{})

	err := e.Kickoff(context.Background(), []byte(`{"repo":"acme/api","report":"r"}`))
	if err == nil {
		t.Fatal("expected the kickoff to fail when the default branch cannot be resolved")
	}
	if !strings.Contains(err.Error(), "default branch") {
		t.Errorf("error = %v, want it to name the unresolved default branch", err)
	}
	if gh.created != nil {
		t.Error("no PR should be opened when the base is unknown")
	}
}
