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

	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/gitrepo"
	"github.com/jkjamies/automation-agent/internal/notify"
)

// --- shared test fakes ---

type fakeGH struct {
	existing     []githubapi.PR
	created      *githubapi.PRInput
	labeled      []string
	findErr      error
	createErr    error
	fileContents map[string]string
	attempts     int
}

func (f *fakeGH) FindAgentPRs(context.Context, string, string, string) ([]githubapi.PR, error) {
	return f.existing, f.findErr
}

func (f *fakeGH) CreatePR(_ context.Context, _, _ string, in githubapi.PRInput) (githubapi.PR, error) {
	if f.createErr != nil {
		return githubapi.PR{}, f.createErr
	}
	f.created = &in
	return githubapi.PR{Number: 42, Branch: in.Head, Title: in.Title, URL: "https://gh/pr/42"}, nil
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

func (f *fakeGH) AttemptCount(context.Context, string, string, int) (int, error) {
	return f.attempts, nil
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

func applyCfg(remote string) ApplyConfig {
	return ApplyConfig{
		Owner: "acme", Repo: "api", CloneURL: remote, Base: "master", Branch: "agent/fix", NewBranch: true,
		Label: "automation-agent", CommitMessage: "fix", PRTitle: "Fix", PRBody: "auto",
		Author: gitrepo.Author{Name: "agent", Email: "a@x"},
	}
}

// --- tests ---

func TestApplyFixCreatesPRAndPushes(t *testing.T) {
	remote := seedRemote(t)
	gh := &fakeGH{}
	res, err := ApplyFix(context.Background(), gh, applyCfg(remote), []FileEdit{{Path: "internal/foo.go", Content: "package foo\n"}})
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
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
	if _, err := ApplyFix(context.Background(), &fakeGH{}, applyCfg(remote), []FileEdit{{Path: "a.go", Content: "package a\n"}}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	retry := applyCfg(remote)
	retry.NewBranch = false
	gh := &fakeGH{existing: []githubapi.PR{{Number: 9, Branch: "agent/fix"}}}
	res, err := ApplyFix(context.Background(), gh, retry, []FileEdit{{Path: "b.go", Content: "package b\n"}})
	if err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	if res.PR.Number != 9 || gh.created != nil {
		t.Errorf("retry should reuse PR #9 without creating: pr=%d created=%v", res.PR.Number, gh.created)
	}
}

func TestApplyFixNoEdits(t *testing.T) {
	if _, err := ApplyFix(context.Background(), &fakeGH{}, applyCfg("x"), nil); err == nil {
		t.Fatal("expected error with no edits")
	}
}

func TestApplyFixCloneError(t *testing.T) {
	bad := applyCfg(filepath.Join(t.TempDir(), "nope"))
	if _, err := ApplyFix(context.Background(), &fakeGH{}, bad, []FileEdit{{Path: "x.go", Content: "package x\n"}}); err == nil {
		t.Fatal("expected clone error")
	}
}

func TestApplyFixCreateError(t *testing.T) {
	gh := &fakeGH{createErr: context.DeadlineExceeded}
	if _, err := ApplyFix(context.Background(), gh, applyCfg(seedRemote(t)), []FileEdit{{Path: "x.go", Content: "package x\n"}}); err == nil {
		t.Fatal("expected error when PR creation fails")
	}
}
