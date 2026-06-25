package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestIsSSHURL(t *testing.T) {
	cases := map[string]bool{
		"git@github.com:acme/api.git":       true,
		"ssh://git@github.com/acme/api.git": true,
		"https://github.com/acme/api.git":   false,
		"http://example.com/acme/api.git":   false,
		"/local/path/repo":                  false,
	}
	for url, want := range cases {
		if got := isSSHURL(url); got != want {
			t.Errorf("isSSHURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestAuthForHTTPS(t *testing.T) {
	// A token yields x-access-token basic auth; an empty token is anonymous (nil).
	m, err := authFor("https://github.com/acme/api.git", Auth{Token: "tok"})
	if err != nil {
		t.Fatalf("authFor https+token: %v", err)
	}
	ba, ok := m.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth = %T, want *http.BasicAuth", m)
	}
	if ba.Username != "x-access-token" || ba.Password != "tok" {
		t.Errorf("basic auth = %s/%s, want x-access-token/tok", ba.Username, ba.Password)
	}
	if m, err := authFor("https://github.com/acme/api.git", Auth{}); err != nil || m != nil {
		t.Errorf("authFor https anonymous = (%v, %v), want (nil, nil)", m, err)
	}
}

func TestAuthForSSHExplicitKeyMissing(t *testing.T) {
	// An ssh URL routes to ssh auth; a non-existent explicit key path is a clear error
	// rather than a silent fallthrough to token auth.
	_, err := authFor("git@github.com:acme/api.git", Auth{SSHKey: filepath.Join(t.TempDir(), "absent_key")})
	if err == nil {
		t.Fatal("expected an error for a missing ssh key file")
	}
}

// seedRemote creates a local repo with one commit to act as the clone source.
func seedRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "s@x", When: time.Unix(1, 0)},
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return dir
}

func TestCloneBranchCommitPush(t *testing.T) {
	remote := seedRemote(t)
	work := filepath.Join(t.TempDir(), "work")
	ctx := context.Background()

	r, err := Clone(ctx, remote, work, Auth{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if err := r.Checkout("agent/fix", true); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(r.Path("fix.txt"), []byte("patched"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha, err := r.CommitAll("apply fix", Author{Name: "agent", Email: "a@x"})
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	head, err := r.Head()
	if err != nil || head != sha {
		t.Fatalf("Head = %q, sha = %q, err = %v", head, sha, err)
	}

	if r.Dir() != work {
		t.Errorf("Dir = %q, want %q", r.Dir(), work)
	}

	if err := r.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// A second push with no new commits is up-to-date, not an error.
	if err := r.Push(ctx); err != nil {
		t.Fatalf("idempotent Push: %v", err)
	}

	// The remote should now have the pushed branch at the committed SHA.
	rr, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := rr.Reference(plumbing.NewBranchReferenceName("agent/fix"), true)
	if err != nil {
		t.Fatalf("remote branch missing: %v", err)
	}
	if ref.Hash().String() != sha {
		t.Errorf("remote agent/fix = %s, want %s", ref.Hash().String(), sha)
	}
}

func TestCheckoutRemote(t *testing.T) {
	remote := seedRemote(t)
	ctx := context.Background()

	// First clone: create and push a branch.
	r1, err := Clone(ctx, remote, filepath.Join(t.TempDir(), "w1"), Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Checkout("feature", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r1.Path("f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := r1.CommitAll("feat", Author{Name: "a", Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Push(ctx); err != nil {
		t.Fatal(err)
	}

	// Second clone: check out the existing remote branch.
	r2, err := Clone(ctx, remote, filepath.Join(t.TempDir(), "w2"), Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.CheckoutRemote("feature"); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	head, _ := r2.Head()
	if head != sha {
		t.Errorf("head = %s, want %s", head, sha)
	}
	if err := r2.CheckoutRemote("does-not-exist"); err == nil {
		t.Error("expected error for missing remote branch")
	}
}

func TestCheckoutMissingBranch(t *testing.T) {
	r, err := Clone(context.Background(), seedRemote(t), filepath.Join(t.TempDir(), "w"), Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("does-not-exist", false); err == nil {
		t.Fatal("expected error checking out a missing branch")
	}
}

func TestCommitNothing(t *testing.T) {
	r, err := Clone(context.Background(), seedRemote(t), filepath.Join(t.TempDir(), "w"), Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitAll("nothing changed", Author{Name: "a", Email: "a@x"}); err == nil {
		t.Fatal("expected error committing a clean tree")
	}
}

func TestCloneBadURL(t *testing.T) {
	work := filepath.Join(t.TempDir(), "nope")
	if _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), work, Auth{}); err == nil {
		t.Fatal("expected clone error for nonexistent source")
	}
}
