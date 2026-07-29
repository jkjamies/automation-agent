package fixflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"automation-agent/internal/githubapi"
	"automation-agent/internal/gitrepo"
	"automation-agent/internal/notify"
)

// --- shared test fakes ---

type fakeGH struct {
	existing     []githubapi.PR
	created      *githubapi.PRInput
	labeled      []string
	findErr      error
	createErr    error
	fileContents map[string]string
	comparison   githubapi.Comparison
	compareErr   error
	// defaultBranch overrides the branch DefaultBranch reports; defaultBranchErr makes the
	// lookup fail, so tests can pin what a kickoff does when the base cannot be resolved.
	defaultBranch    string
	defaultBranchErr error
}

func (f *fakeGH) Compare(_ context.Context, _, _, _, _ string) (githubapi.Comparison, error) {
	return f.comparison, f.compareErr
}

func (f *fakeGH) FindOpenPRByBranch(_ context.Context, _, _, branch string) (githubapi.PR, bool, error) {
	if f.findErr != nil {
		return githubapi.PR{}, false, f.findErr
	}
	for _, pr := range f.existing {
		if pr.Branch == branch {
			return pr, true, nil
		}
	}
	return githubapi.PR{}, false, nil
}

func (f *fakeGH) CreatePR(_ context.Context, _, _ string, in githubapi.PRInput) (githubapi.PR, error) {
	if f.createErr != nil {
		return githubapi.PR{}, f.createErr
	}
	f.created = &in
	return githubapi.PR{Number: 42, Branch: in.Head, Title: in.Title, URL: "https://gh/pr/42"}, nil
}

func (f *fakeGH) DefaultBranch(_ context.Context, _, _ string) (string, error) {
	if f.defaultBranchErr != nil {
		return "", f.defaultBranchErr
	}
	if f.defaultBranch != "" {
		return f.defaultBranch, nil
	}
	return "master", nil // what git.PlainInit gives the seeded remotes
}

func (f *fakeGH) AddLabels(_ context.Context, _, _ string, _ int, labels ...string) error {
	f.labeled = append(f.labeled, labels...)
	return nil
}

func (f *fakeGH) GetFileContent(_ context.Context, _, _, path, _ string) (string, error) {
	if c, ok := f.fileContents[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("no content for %s", path)
}

type fakeNotifier struct{ msgs []notify.Message }

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.msgs = append(n.msgs, m)
	return nil
}

func seedRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, _ := repo.Worktree()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "s@x", When: time.Unix(1, 0)},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dir
}

// seedRemoteWithBranch seeds a remote whose default branch is master and which also carries
// `branch`, holding one file master does not. That marker file is how a test proves which ref
// a fix branch was actually cut from.
func seedRemoteWithBranch(t *testing.T, branch string) string {
	t.Helper()
	dir := seedRemote(t)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open seeded remote: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Create: true}); err != nil {
		t.Fatalf("create %s: %v", branch, err)
	}
	marker := "only-on-" + branch + ".txt"
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("branch-only change", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "s@x", When: time.Unix(2, 0)},
	}); err != nil {
		t.Fatalf("commit on %s: %v", branch, err)
	}
	// Leave the remote's HEAD on master so a clone that ignores the requested base lands
	// there — which is exactly the mistake the test is looking for.
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}); err != nil {
		t.Fatalf("restore master: %v", err)
	}
	return dir
}

// branchHasFile reports whether path exists in the tree of branch on the given repo.
func branchHasFile(t *testing.T, dir, branch, path string) bool {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve %s: %v", branch, err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit for %s: %v", branch, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree for %s: %v", branch, err)
	}
	_, err = tree.File(path)
	return err == nil
}

func applyCfg(remote string) applyConfig {
	return applyConfig{
		Owner: "acme", Repo: "api", CloneURL: remote, Base: "master", Branch: "agent/fix",
		Label: "automation-agent", CommitMessage: "fix", PRTitle: "Fix", PRBody: "auto",
		Author: gitrepo.Author{Name: "agent", Email: "a@x"},
	}
}

// --- tests ---

func TestApplyFixCreatesPRAndPushes(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	res, err := applyFix(context.Background(), gh, applyCfg(remote), []FileEdit{{Path: "internal/foo.go", Content: "package foo\n"}})
	if err != nil {
		t.Fatalf("applyFix: %v", err)
	}
	if res.PR.Number != 42 || res.HeadSHA == "" {
		t.Errorf("result = %+v", res)
	}
	if gh.created == nil || gh.created.Head != "agent/fix" {
		t.Errorf("create input = %+v", gh.created)
	}
	if len(gh.labeled) != 1 || gh.labeled[0] != "automation-agent" {
		t.Errorf("labels = %v", gh.labeled)
	}
	rr, _ := git.PlainOpen(remote)
	if _, err := rr.Reference(plumbing.NewBranchReferenceName("agent/fix"), true); err != nil {
		t.Fatalf("remote branch missing: %v", err)
	}
}

func TestApplyFixRetryReusesBranch(t *testing.T) {
	remote := seedRemote(t)
	if _, err := applyFix(context.Background(), &fakeGH{}, applyCfg(remote), []FileEdit{{Path: "a.go", Content: "package a\n"}}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	retry := applyCfg(remote)
	gh := &fakeGH{existing: []githubapi.PR{{Number: 9, Branch: "agent/fix"}}}
	res, err := applyFix(context.Background(), gh, retry, []FileEdit{{Path: "b.go", Content: "package b\n"}})
	if err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	if res.PR.Number != 9 || gh.created != nil {
		t.Errorf("retry should reuse PR #9 without creating: pr=%d created=%v", res.PR.Number, gh.created)
	}
}

// A re-apply of already-correct content (the LLM re-emits a file that matches the branch)
// is a benign no-op: it resolves as a successful apply (PR ensured, HEAD reused), not a
// failed fix. Regression guard for the G1 ErrNoChanges branch in Commit.
func TestApplyFixNoOpEditSucceeds(t *testing.T) {
	remote := seedRemote(t)
	edits := []FileEdit{{Path: "a.go", Content: "package a\n"}}
	first, err := applyFix(context.Background(), &fakeGH{}, applyCfg(remote), edits)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Re-apply identical content on the existing branch: CommitAll has nothing to commit.
	retry := applyCfg(remote)
	gh := &fakeGH{existing: []githubapi.PR{{Number: 7, Branch: "agent/fix"}}}
	res, err := applyFix(context.Background(), gh, retry, edits)
	if err != nil {
		t.Fatalf("no-op re-apply should succeed, got: %v", err)
	}
	if res.PR.Number != 7 {
		t.Errorf("no-op apply should reuse PR #7, got %d", res.PR.Number)
	}
	if res.HeadSHA != first.HeadSHA {
		t.Errorf("no-op apply HeadSHA = %q, want unchanged %q", res.HeadSHA, first.HeadSHA)
	}
}

func TestApplyFixNoEdits(t *testing.T) {
	if _, err := applyFix(context.Background(), &fakeGH{}, applyCfg("x"), nil); err == nil {
		t.Fatal("expected error with no edits")
	}
}

func TestApplyFixCloneError(t *testing.T) {
	bad := applyCfg(filepath.Join(t.TempDir(), "nope"))
	if _, err := applyFix(context.Background(), &fakeGH{}, bad, []FileEdit{{Path: "x.go", Content: "package x\n"}}); err == nil {
		t.Fatal("expected clone error")
	}
}

func TestApplyFixCreateError(t *testing.T) {
	gh := &fakeGH{createErr: context.DeadlineExceeded}
	if _, err := applyFix(context.Background(), gh, applyCfg(seedRemote(t)), []FileEdit{{Path: "x.go", Content: "package x\n"}}); err == nil {
		t.Fatal("expected error when PR creation fails")
	}
}
