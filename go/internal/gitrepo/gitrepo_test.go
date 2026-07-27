package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// fakeProvider is a TokenProvider returning a fixed token, recording the repo it
// was asked for so tests can assert the per-op token is scoped to the right repo.
type fakeProvider struct {
	tok      string
	lastRepo string
	calls    int
}

func (f *fakeProvider) Token(_ context.Context, repo string) (string, error) {
	f.calls++
	f.lastRepo = repo
	return f.tok, nil
}

func TestAuthForHTTPS(t *testing.T) {
	// A provider token yields x-access-token basic auth, scoped to the repo; a nil
	// provider is anonymous (nil).
	p := &fakeProvider{tok: "tok"}
	m, err := authFor(context.Background(), "https://github.com/acme/api.git", Auth{Provider: p, Repo: "acme/api"})
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
	if p.lastRepo != "acme/api" {
		t.Errorf("provider asked for repo %q, want acme/api", p.lastRepo)
	}
	if m, err := authFor(context.Background(), "https://github.com/acme/api.git", Auth{}); err != nil || m != nil {
		t.Errorf("authFor https anonymous = (%v, %v), want (nil, nil)", m, err)
	}
}

func TestAuthForRefusesPlaintextHTTP(t *testing.T) {
	// A plaintext http remote must never receive a token as Basic Auth — refuse before
	// any token lookup so credentials can't leak over an unencrypted transport.
	p := &fakeProvider{tok: "tok"}
	if _, err := authFor(context.Background(), "http://example.com/acme/api.git", Auth{Provider: p, Repo: "acme/api"}); err == nil {
		t.Fatal("expected an error for an http:// remote, got nil")
	}
	if p.calls != 0 {
		t.Errorf("provider.Token called %d times for http remote, want 0", p.calls)
	}
}

func TestAuthForSSHExplicitKeyMissing(t *testing.T) {
	// An ssh URL routes to ssh auth; a non-existent explicit key path is a clear error
	// rather than a silent fallthrough to token auth.
	_, err := authFor(context.Background(), "git@github.com:acme/api.git", Auth{SSHKey: filepath.Join(t.TempDir(), "absent_key")})
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

// TestAuthForReResolvesPerOp verifies the per-op token contract directly on authFor
// (the single resolution point Clone and Push both call): each invocation against an
// https remote re-fetches from the provider, so a short-lived installation token is
// re-read rather than captured stale. The https token path itself is asserted in
// TestAuthForHTTPS.
func TestAuthForReResolvesPerOp(t *testing.T) {
	p := &fakeProvider{tok: "tok"}
	a := Auth{Provider: p, Repo: "acme/api"}
	for i := 0; i < 2; i++ {
		if _, err := authFor(context.Background(), "https://github.com/acme/api.git", a); err != nil {
			t.Fatalf("authFor #%d: %v", i, err)
		}
	}
	if p.calls != 2 {
		t.Errorf("provider consulted %d times over 2 ops, want 2 (per-op re-resolution)", p.calls)
	}
}

// TestLocalRemoteSkipsProvider guards the fix that local/file remotes never mint a
// token: a local seed remote clones and pushes anonymously, without consulting the
// provider (which in App mode would be a needless GitHub installation-token request).
func TestLocalRemoteSkipsProvider(t *testing.T) {
	remote := seedRemote(t)
	work := filepath.Join(t.TempDir(), "work")
	ctx := context.Background()
	p := &fakeProvider{tok: "tok"}

	r, err := Clone(ctx, remote, work, "", Auth{Provider: p, Repo: "acme/api"})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := r.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if p.calls != 0 {
		t.Errorf("provider consulted %d times for a local remote, want 0 (anonymous)", p.calls)
	}
}

func TestCloneBranchCommitPush(t *testing.T) {
	remote := seedRemote(t)
	work := filepath.Join(t.TempDir(), "work")
	ctx := context.Background()

	r, err := Clone(ctx, remote, work, "", Auth{})
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
	r1, err := Clone(ctx, remote, filepath.Join(t.TempDir(), "w1"), "", Auth{})
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
	r2, err := Clone(ctx, remote, filepath.Join(t.TempDir(), "w2"), "", Auth{})
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
	r, err := Clone(context.Background(), seedRemote(t), filepath.Join(t.TempDir(), "w"), "", Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("does-not-exist", false); err == nil {
		t.Fatal("expected error checking out a missing branch")
	}
}

func TestCommitNothing(t *testing.T) {
	r, err := Clone(context.Background(), seedRemote(t), filepath.Join(t.TempDir(), "w"), "", Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitAll("nothing changed", Author{Name: "a", Email: "a@x"}); err == nil {
		t.Fatal("expected error committing a clean tree")
	}
}

func TestCloneBadURL(t *testing.T) {
	work := filepath.Join(t.TempDir(), "nope")
	if _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), work, "", Auth{}); err == nil {
		t.Fatal("expected clone error for nonexistent source")
	}
}

// seedRemoteWithBranch seeds a remote whose default branch is master and which also carries
// `branch`, holding a file master does not — the marker that proves which ref was checked out.
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
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("marker.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("branch-only change", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "s@x", When: time.Unix(2, 0)},
	}); err != nil {
		t.Fatalf("commit on %s: %v", branch, err)
	}
	// Leave the remote's HEAD on master, so a clone that ignores the requested branch lands
	// there — which is exactly what these tests are watching for.
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}); err != nil {
		t.Fatalf("restore master: %v", err)
	}
	return dir
}

// Clone checks out the requested branch rather than the remote's default. The fixers depend
// on this: the branch a fix is cut from must be the same ref its PR targets, or the PR
// carries every commit between the two.
func TestCloneChecksOutRequestedBranch(t *testing.T) {
	remote := seedRemoteWithBranch(t, "develop")
	work := filepath.Join(t.TempDir(), "w")

	r, err := Clone(context.Background(), remote, work, "develop", Auth{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(r.Path("marker.txt")); err != nil {
		t.Errorf("checkout is missing develop's marker file, so it landed on another branch: %v", err)
	}
	head, err := r.repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if got := head.Name().Short(); got != "develop" {
		t.Errorf("HEAD = %q, want develop", got)
	}
}

// An empty branch keeps the remote's default — the behavior callers that just want HEAD rely on.
func TestCloneEmptyBranchUsesRemoteDefault(t *testing.T) {
	remote := seedRemoteWithBranch(t, "develop")
	work := filepath.Join(t.TempDir(), "w")

	r, err := Clone(context.Background(), remote, work, "", Auth{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(r.Path("marker.txt")); err == nil {
		t.Error("an empty branch should follow the remote default (master), not develop")
	}
}

// A branch that does not exist is an error naming the branch, not a silent fall back to the
// default — falling back would branch the fix off the wrong ref.
func TestCloneUnknownBranchErrors(t *testing.T) {
	remote := seedRemote(t)
	work := filepath.Join(t.TempDir(), "w")

	_, err := Clone(context.Background(), remote, work, "no-such-branch", Auth{})
	if err == nil {
		t.Fatal("expected an error cloning a branch that does not exist")
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Errorf("error = %v, want it to name the missing branch", err)
	}
}

// CheckoutOrCreate continues a branch the remote already has, so an apply that repeats
// (a retry, or a redelivered kickoff) adds to the existing branch instead of trying to
// recreate it from the base — which would be rejected at push as non-fast-forward.
func TestCheckoutOrCreateContinuesExistingRemoteBranch(t *testing.T) {
	remote := seedRemoteWithBranch(t, "agent/fix")
	work := filepath.Join(t.TempDir(), "w")
	r, err := Clone(context.Background(), remote, work, "master", Auth{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	existed, err := r.CheckoutOrCreate("agent/fix")
	if err != nil {
		t.Fatalf("CheckoutOrCreate: %v", err)
	}
	if !existed {
		t.Error("existed = false, want true for a branch the remote already has")
	}
	// Continuing means the branch's own content is in the tree, not the base's.
	if _, err := os.Stat(r.Path("marker.txt")); err != nil {
		t.Errorf("checkout is missing the existing branch's content: %v", err)
	}
}

// An unknown branch is created from the current HEAD (the base the clone checked out).
func TestCheckoutOrCreateCreatesMissingBranch(t *testing.T) {
	remote := seedRemote(t)
	work := filepath.Join(t.TempDir(), "w")
	r, err := Clone(context.Background(), remote, work, "", Auth{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	existed, err := r.CheckoutOrCreate("agent/brand-new")
	if err != nil {
		t.Fatalf("CheckoutOrCreate: %v", err)
	}
	if existed {
		t.Error("existed = true, want false for a branch the remote does not have")
	}
	head, err := r.repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if got := head.Name().Short(); got != "agent/brand-new" {
		t.Errorf("HEAD = %q, want the newly created branch", got)
	}
}
