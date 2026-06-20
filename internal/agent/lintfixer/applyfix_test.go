package lintfixer

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

	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/gitrepo"
)

// seedRemote creates a local repo with one commit to clone from.
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

type fakeGH struct {
	existing     []githubapi.PR
	created      *githubapi.PRInput
	labeled      []string
	labelNumber  int
	findErr      error
	createErr    error
	fileContents map[string]string
	attempts     int
}

func (f *fakeGH) FindAgentPRs(context.Context, string, string, string) ([]githubapi.PR, error) {
	return f.existing, f.findErr
}

func (f *fakeGH) GetFileContent(_ context.Context, _, _, path, _ string) (string, error) {
	if c, ok := f.fileContents[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("no content for %s", path)
}

func (f *fakeGH) AttemptCount(context.Context, string, string, int) (int, error) {
	return f.attempts, nil
}

func (f *fakeGH) CreatePR(_ context.Context, _, _ string, in githubapi.PRInput) (githubapi.PR, error) {
	if f.createErr != nil {
		return githubapi.PR{}, f.createErr
	}
	f.created = &in
	return githubapi.PR{Number: 42, Branch: in.Head, Title: in.Title, URL: "https://gh/pr/42"}, nil
}

func (f *fakeGH) AddLabels(_ context.Context, _, _ string, number int, labels ...string) error {
	f.labelNumber = number
	f.labeled = append(f.labeled, labels...)
	return nil
}

func cfg(remote string) ApplyConfig {
	return ApplyConfig{
		Owner: "acme", Repo: "api", CloneURL: remote, Base: "master", Branch: "agent/fix", NewBranch: true,
		Label: "automation-agent", CommitMessage: "fix lint", PRTitle: "Fix lint", PRBody: "auto",
		Author: gitrepo.Author{Name: "agent", Email: "a@x"},
	}
}

func TestApplyFixCreatesPRAndPushes(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}

	res, err := ApplyFix(context.Background(), gh, cfg(remote), []FileEdit{
		{Path: "internal/foo.go", Content: "package foo\n"},
	})
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if res.PR.Number != 42 || res.HeadSHA == "" {
		t.Errorf("result = %+v", res)
	}
	if gh.created == nil || gh.created.Head != "agent/fix" || gh.created.Base != "master" {
		t.Errorf("create input = %+v", gh.created)
	}
	if len(gh.labeled) != 1 || gh.labeled[0] != "automation-agent" || gh.labelNumber != 42 {
		t.Errorf("labels = %v on #%d", gh.labeled, gh.labelNumber)
	}

	// The remote should now have the agent branch carrying the edit.
	rr, _ := git.PlainOpen(remote)
	ref, err := rr.Reference(plumbing.NewBranchReferenceName("agent/fix"), true)
	if err != nil {
		t.Fatalf("remote branch missing: %v", err)
	}
	if ref.Hash().String() != res.HeadSHA {
		t.Errorf("remote head = %s, want %s", ref.Hash().String(), res.HeadSHA)
	}
}

func TestApplyFixReusesExistingPR(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{existing: []githubapi.PR{{Number: 7, Branch: "agent/fix"}}}

	res, err := ApplyFix(context.Background(), gh, cfg(remote), []FileEdit{
		{Path: "x.go", Content: "package x\n"},
	})
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if res.PR.Number != 7 {
		t.Errorf("expected to reuse PR #7, got #%d", res.PR.Number)
	}
	if gh.created != nil {
		t.Error("should not create a new PR when one exists for the branch")
	}
}

func TestApplyFixRetryReusesBranch(t *testing.T) {
	remote := seedRemote(t)
	ctx := context.Background()

	// Kickoff creates the branch on the remote.
	if _, err := ApplyFix(ctx, &fakeGH{}, cfg(remote), []FileEdit{{Path: "a.go", Content: "package a\n"}}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Retry checks out the existing remote branch and adds a commit; PR is reused.
	retry := cfg(remote)
	retry.NewBranch = false
	retry.CommitMessage = "second fix"
	gh := &fakeGH{existing: []githubapi.PR{{Number: 9, Branch: "agent/fix"}}}
	res, err := ApplyFix(ctx, gh, retry, []FileEdit{{Path: "b.go", Content: "package b\n"}})
	if err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	if res.PR.Number != 9 {
		t.Errorf("retry should reuse PR #9, got #%d", res.PR.Number)
	}
	if gh.created != nil {
		t.Error("retry should not create a new PR")
	}

	// The remote branch should now carry seed + first fix + second fix = 3 commits.
	rr, _ := git.PlainOpen(remote)
	ref, _ := rr.Reference(plumbing.NewBranchReferenceName("agent/fix"), true)
	commit, err := rr.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	logIter, _ := rr.Log(&git.LogOptions{From: commit.Hash})
	_ = logIter.ForEach(func(*object.Commit) error { count++; return nil })
	if count != 3 {
		t.Errorf("branch has %d commits, want 3", count)
	}
}

func TestApplyFixNoEdits(t *testing.T) {
	if _, err := ApplyFix(context.Background(), &fakeGH{}, cfg("x"), nil); err == nil {
		t.Fatal("expected error with no edits")
	}
}

func TestApplyFixCloneError(t *testing.T) {
	bad := cfg(filepath.Join(t.TempDir(), "nonexistent"))
	_, err := ApplyFix(context.Background(), &fakeGH{}, bad, []FileEdit{{Path: "x.go", Content: "package x\n"}})
	if err == nil {
		t.Fatal("expected clone error for nonexistent source")
	}
}

func TestApplyFixCreatePRError(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{createErr: context.DeadlineExceeded}
	_, err := ApplyFix(context.Background(), gh, cfg(remote), []FileEdit{{Path: "x.go", Content: "package x\n"}})
	if err == nil {
		t.Fatal("expected error when PR creation fails")
	}
}

func TestApplyFixFindError(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{findErr: context.DeadlineExceeded}
	_, err := ApplyFix(context.Background(), gh, cfg(remote), []FileEdit{{Path: "x.go", Content: "package x\n"}})
	if err == nil {
		t.Fatal("expected error when listing PRs fails")
	}
}
