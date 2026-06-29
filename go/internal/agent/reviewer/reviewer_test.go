package reviewer

import (
	"context"
	"errors"
	"testing"

	"automation-agent/internal/githubapi"
)

// fakeGH is a stub gitHubClient: it returns canned files (or an error) and counts calls so a
// test can assert whether intake reached the fetch step.
type fakeGH struct {
	files []githubapi.PRFile
	err   error
	calls int
}

func (f *fakeGH) ListPRFiles(context.Context, string, string, int) ([]githubapi.PRFile, error) {
	f.calls++
	return f.files, f.err
}

// testEngine builds an enabled engine with small caps and a default exclude set, overridable.
func testEngine(gh gitHubClient, mut ...func(*Deps)) *Engine {
	d := Deps{
		Enabled:      true,
		GH:           gh,
		SkipDrafts:   true,
		ExcludeGlobs: []string{"go.sum", "vendor/**"},
		MaxFiles:     50,
		MaxDiffBytes: 1000,
	}
	for _, m := range mut {
		m(&d)
	}
	return NewEngine(d)
}

func event(action string, mut ...func(*githubapi.PullRequestEvent)) githubapi.PullRequestEvent {
	e := githubapi.PullRequestEvent{Action: action, Number: 1, RepoFullName: "o/r", HeadRef: "feature/x"}
	for _, m := range mut {
		m(&e)
	}
	return e
}

// When disabled (the default), Kickoff acknowledges the event and does no work — not even
// parsing, so a garbage body is fine.
func TestKickoffDisabledNoOp(t *testing.T) {
	gh := &fakeGH{}
	e := NewEngine(Deps{Enabled: false, GH: gh})
	if err := e.Kickoff(context.Background(), []byte("not even json")); err != nil {
		t.Fatalf("disabled Kickoff returned error: %v", err)
	}
	if gh.calls != 0 {
		t.Error("disabled engine must not fetch files")
	}
}

// NewEngine tolerates a nil logger (falls back to the default) rather than panicking.
func TestNewEngineNilLogger(t *testing.T) {
	e := NewEngine(Deps{Enabled: false, GH: &fakeGH{}, Log: nil})
	if err := e.Kickoff(context.Background(), nil); err != nil {
		t.Fatalf("Kickoff with default logger returned error: %v", err)
	}
}

// An enabled engine with no GitHub client returns a controlled error instead of panicking
// on a nil-client dereference in decide.
func TestKickoffEnabledNilClient(t *testing.T) {
	e := NewEngine(Deps{Enabled: true, GH: nil})
	body := `{"action":"opened","pull_request":{"number":1,"head":{"ref":"feature/x"}},"repository":{"full_name":"o/r"}}`
	if err := e.Kickoff(context.Background(), []byte(body)); err == nil {
		t.Fatal("expected an error when enabled with no GitHub client")
	}
}

// Kickoff parses the event and runs intake to a review plan on a normal PR.
func TestKickoffEnabledReviews(t *testing.T) {
	gh := &fakeGH{files: []githubapi.PRFile{{Path: "main.go", Patch: "abc"}}}
	e := testEngine(gh)
	body := `{"action":"opened","pull_request":{"number":3,"head":{"ref":"feature/x","sha":"s"},"base":{"ref":"main"},"user":{"login":"alice"}},"repository":{"full_name":"o/r"}}`
	if err := e.Kickoff(context.Background(), []byte(body)); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("ListPRFiles calls = %d, want 1", gh.calls)
	}
}

func TestKickoffMalformedBody(t *testing.T) {
	e := testEngine(&fakeGH{})
	if err := e.Kickoff(context.Background(), []byte("{bad")); err == nil {
		t.Fatal("expected an error for a malformed pull_request body")
	}
}

// decide covers the trigger/skip matrix and the filter/size-gate outcomes.
func TestDecide(t *testing.T) {
	realFile := []githubapi.PRFile{{Path: "main.go", Patch: "abc"}}

	t.Run("untriggered action skips before fetch", func(t *testing.T) {
		gh := &fakeGH{files: realFile}
		d, err := testEngine(gh).decide(context.Background(), event("closed"))
		if err != nil || d.kind != decisionSkip {
			t.Fatalf("decide = %+v, err %v; want skip", d, err)
		}
		if gh.calls != 0 {
			t.Error("must not fetch for an untriggered action")
		}
	})

	t.Run("draft skipped unless ready_for_review", func(t *testing.T) {
		gh := &fakeGH{files: realFile}
		d, _ := testEngine(gh).decide(context.Background(), event("opened", func(e *githubapi.PullRequestEvent) { e.Draft = true }))
		if d.kind != decisionSkip || gh.calls != 0 {
			t.Fatalf("draft opened: decide = %+v, calls %d; want skip pre-fetch", d, gh.calls)
		}
	})

	t.Run("draft reviewed on ready_for_review", func(t *testing.T) {
		gh := &fakeGH{files: realFile}
		d, err := testEngine(gh).decide(context.Background(), event("ready_for_review", func(e *githubapi.PullRequestEvent) { e.Draft = true }))
		if err != nil || d.kind != decisionReview {
			t.Fatalf("ready_for_review draft: decide = %+v, err %v; want review", d, err)
		}
	})

	t.Run("own agent PR skips", func(t *testing.T) {
		gh := &fakeGH{files: realFile}
		d, _ := testEngine(gh).decide(context.Background(), event("opened", func(e *githubapi.PullRequestEvent) { e.HeadRef = "automation-agent/lint-fix" }))
		if d.kind != decisionSkip || gh.calls != 0 {
			t.Fatalf("own PR: decide = %+v; want skip pre-fetch", d)
		}
	})

	t.Run("skip-review label skips", func(t *testing.T) {
		gh := &fakeGH{files: realFile}
		d, _ := testEngine(gh).decide(context.Background(), event("opened", func(e *githubapi.PullRequestEvent) { e.Labels = []string{"skip-review"} }))
		if d.kind != decisionSkip {
			t.Fatalf("labelled: decide = %+v; want skip", d)
		}
	})

	t.Run("dependency bot skips", func(t *testing.T) {
		gh := &fakeGH{files: realFile}
		d, _ := testEngine(gh).decide(context.Background(), event("opened", func(e *githubapi.PullRequestEvent) { e.AuthorLogin = "dependabot[bot]" }))
		if d.kind != decisionSkip {
			t.Fatalf("dependabot: decide = %+v; want skip", d)
		}
	})

	t.Run("empty filtered diff skips", func(t *testing.T) {
		gh := &fakeGH{files: []githubapi.PRFile{{Path: "go.sum", Patch: "x"}, {Path: "vendor/y.go", Patch: "x"}}}
		d, _ := testEngine(gh).decide(context.Background(), event("opened"))
		if d.kind != decisionSkip || gh.calls != 1 {
			t.Fatalf("all-excluded: decide = %+v, calls %d; want skip after fetch", d, gh.calls)
		}
	})

	t.Run("normal PR reviews with filtered size", func(t *testing.T) {
		gh := &fakeGH{files: []githubapi.PRFile{{Path: "main.go", Patch: "12345"}, {Path: "go.sum", Patch: "ignored"}}}
		d, err := testEngine(gh).decide(context.Background(), event("synchronize"))
		if err != nil || d.kind != decisionReview {
			t.Fatalf("decide = %+v, err %v; want review", d, err)
		}
		if len(d.files) != 1 || d.diffBytes != 5 {
			t.Errorf("plan = %d files / %d bytes, want 1 / 5 (go.sum excluded)", len(d.files), d.diffBytes)
		}
	})

	t.Run("oversize PR is denied", func(t *testing.T) {
		gh := &fakeGH{files: []githubapi.PRFile{{Path: "a.go", Patch: "x"}, {Path: "b.go", Patch: "x"}}}
		d, _ := testEngine(gh, func(d *Deps) { d.MaxFiles = 1 }).decide(context.Background(), event("opened"))
		if d.kind != decisionDeny || d.reason == "" {
			t.Fatalf("decide = %+v; want deny with a reason", d)
		}
	})

	t.Run("malformed repo full name errors", func(t *testing.T) {
		_, err := testEngine(&fakeGH{files: realFile}).decide(context.Background(), event("opened", func(e *githubapi.PullRequestEvent) { e.RepoFullName = "noslash" }))
		if err == nil {
			t.Fatal("expected an error for a malformed repository full name")
		}
	})

	t.Run("list-files error propagates", func(t *testing.T) {
		gh := &fakeGH{err: errors.New("boom")}
		_, err := testEngine(gh).decide(context.Background(), event("opened"))
		if err == nil {
			t.Fatal("expected the ListPRFiles error to propagate")
		}
	})
}

func TestSplitFullName(t *testing.T) {
	cases := map[string]struct {
		owner, repo string
		ok          bool
	}{
		"o/r":      {"o", "r", true},
		"acme/web": {"acme", "web", true},
		"noslash":  {"", "", false},
		"a/b/c":    {"", "", false},
		"/r":       {"", "", false},
		"o/":       {"", "", false},
	}
	for in, want := range cases {
		owner, repo, ok := splitFullName(in)
		if owner != want.owner || repo != want.repo || ok != want.ok {
			t.Errorf("splitFullName(%q) = %q,%q,%v; want %q,%q,%v", in, owner, repo, ok, want.owner, want.repo, want.ok)
		}
	}
}
