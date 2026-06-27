// Package gitrepo wraps go-git for the working-tree operations the lint-fixer
// needs: clone, branch, stage-all, commit, push. Pure-Go (no git binary).
// Deterministic tooling — no agent imports.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gossh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Author identifies the committer.
type Author struct {
	Name  string
	Email string
}

// Repo is a cloned working tree. It keeps the clone URL and Auth so Push can
// re-resolve credentials per operation — GitHub App installation tokens are
// short-lived (~1h), so a token captured at clone time may be stale by push.
type Repo struct {
	repo *git.Repository
	wt   *git.Worktree
	dir  string
	url  string
	auth Auth
	now  func() time.Time
}

// TokenProvider yields a valid GitHub token for a repo ("owner/name"), re-fetched
// per git operation. It is the gitrepo-local view of auth.TokenProvider (a narrow
// interface kept here so gitrepo stays decoupled from the auth package).
type TokenProvider interface {
	Token(ctx context.Context, repo string) (string, error)
}

// Auth carries the credentials Clone/Push use. Which one applies is chosen by the
// clone URL scheme, not by the caller: an https remote uses Provider (GitHub
// x-access-token basic auth), an ssh remote (git@… / ssh://…) uses SSHKey or the
// ssh-agent.
type Auth struct {
	// Provider yields the GitHub token used as x-access-token basic auth on https
	// remotes, fetched fresh per git op (scoped to Repo) so a short-lived
	// installation token is always current. Nil — or a token of "" — means
	// anonymous (public read only). Ignored for ssh remotes.
	Provider TokenProvider
	// Repo is "owner/name", passed to Provider so App mode can scope the token.
	Repo string
	// SSHKey is an explicit private-key path for ssh remotes. Empty falls back to the
	// ssh-agent, then the default identity files (~/.ssh/id_ed25519, id_rsa, id_ecdsa),
	// mirroring the ssh binary. Ignored for https remotes.
	SSHKey string
}

// sshUser is the user every GitHub ssh remote authenticates as (git@github.com).
const sshUser = "git"

// isSSHURL reports whether url is an scp-style (git@host:path) or ssh:// remote, as
// opposed to an https remote. The agent only ever builds these two forms.
func isSSHURL(url string) bool {
	return strings.HasPrefix(url, "ssh://") || strings.HasPrefix(url, "git@")
}

// authFor selects the auth method by URL scheme: ssh keys/agent for an ssh remote,
// x-access-token basic auth (or nil/anonymous) for an https remote. For https it
// fetches a fresh, repo-scoped token from the provider at call time, so each git op
// uses a currently-valid token. Host-key checking stays on for ssh — go-git defaults
// HostKeyCallback to the user's known_hosts.
func authFor(ctx context.Context, url string, a Auth) (transport.AuthMethod, error) {
	if isSSHURL(url) {
		return sshAuth(a.SSHKey)
	}
	// Only https remotes use token auth. Refuse plaintext http — sending a PAT/App
	// token as Basic Auth over an unencrypted transport would leak it. A local path
	// or file:// remote needs no credentials, and fetching a token for one would mint
	// a needless GitHub installation token (a real API round-trip) in App mode.
	if strings.HasPrefix(url, "http://") {
		return nil, errors.New("refusing to send GitHub token over insecure http remote; use https or ssh")
	}
	if !strings.HasPrefix(url, "https://") {
		return nil, nil
	}
	if a.Provider == nil {
		return nil, nil
	}
	tok, err := a.Provider.Token(ctx, a.Repo)
	if err != nil {
		return nil, fmt.Errorf("get token for %s: %w", a.Repo, err)
	}
	if tok == "" {
		return nil, nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: tok}, nil
}

// sshAuth resolves ssh credentials like the ssh binary: an explicit key path wins;
// otherwise prefer a running ssh-agent (which handles passphrase-protected and hardware
// keys), then fall back to the first default identity file present.
func sshAuth(keyPath string) (transport.AuthMethod, error) {
	if keyPath != "" {
		m, err := gossh.NewPublicKeysFromFile(sshUser, keyPath, "")
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w", keyPath, err)
		}
		return m, nil
	}
	if m, err := gossh.NewSSHAgentAuth(sshUser); err == nil {
		return m, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("ssh: locate home dir: %w", err)
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		m, err := gossh.NewPublicKeysFromFile(sshUser, p, "")
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w", p, err)
		}
		return m, nil
	}
	return nil, errors.New("ssh: no ssh-agent and no default identity file " +
		"(~/.ssh/id_ed25519|id_rsa|id_ecdsa); set GIT_SSH_KEY or start ssh-agent")
}

// Clone clones url into dir (which must not already exist). The auth applied is chosen
// by url's scheme — see Auth.
func Clone(ctx context.Context, url, dir string, auth Auth) (*Repo, error) {
	am, err := authFor(ctx, url, auth)
	if err != nil {
		return nil, fmt.Errorf("auth for %s: %w", url, err)
	}
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: url, Auth: am})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", url, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	return &Repo{repo: repo, wt: wt, dir: dir, url: url, auth: auth, now: time.Now}, nil
}

// Dir returns the working-tree directory; callers write file edits under it.
func (r *Repo) Dir() string { return r.dir }

// Path joins rel onto the working-tree directory.
func (r *Repo) Path(rel string) string { return filepath.Join(r.dir, rel) }

// Checkout switches to branch, creating it from the current HEAD when create is true.
func (r *Repo) Checkout(branch string, create bool) error {
	if err := r.wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: create,
	}); err != nil {
		return fmt.Errorf("checkout %s: %w", branch, err)
	}
	return nil
}

// CheckoutRemote checks out an existing remote branch (origin/<branch>) as a local
// branch — used on retry iterations to add a commit onto the previous fix rather
// than starting a new branch from the base.
func (r *Repo) CheckoutRemote(branch string) error {
	remoteRef := plumbing.NewRemoteReferenceName("origin", branch)
	ref, err := r.repo.Reference(remoteRef, true)
	if err != nil {
		return fmt.Errorf("resolve origin/%s: %w", branch, err)
	}
	local := plumbing.NewBranchReferenceName(branch)
	if err := r.repo.Storer.SetReference(plumbing.NewHashReference(local, ref.Hash())); err != nil {
		return fmt.Errorf("create local branch %s: %w", branch, err)
	}
	if err := r.wt.Checkout(&git.CheckoutOptions{Branch: local}); err != nil {
		return fmt.Errorf("checkout %s: %w", branch, err)
	}
	return nil
}

// ErrNoChanges is returned by CommitAll when the working tree is clean (the edits
// produced no actual change), so callers can distinguish "nothing to do" from a
// real failure.
var ErrNoChanges = errors.New("gitrepo: no changes to commit")

// CommitAll stages every change (including deletions) and commits, returning the
// new commit SHA. It returns ErrNoChanges if the tree is clean.
func (r *Repo) CommitAll(msg string, a Author) (string, error) {
	if err := r.wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", fmt.Errorf("stage changes: %w", err)
	}
	status, err := r.wt.Status()
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	if status.IsClean() {
		return "", ErrNoChanges
	}
	h, err := r.wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: a.Name, Email: a.Email, When: r.now()},
	})
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return h.String(), nil
}

// Push pushes the current branch to origin. An up-to-date push is not an error.
// Credentials are re-resolved here (not reused from clone) so a fresh, repo-scoped
// token authenticates the push even if the clone-time token has since expired.
func (r *Repo) Push(ctx context.Context) error {
	am, err := authFor(ctx, r.url, r.auth)
	if err != nil {
		return fmt.Errorf("auth for push: %w", err)
	}
	err = r.repo.PushContext(ctx, &git.PushOptions{Auth: am})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// Head returns the current HEAD commit SHA.
func (r *Repo) Head() (string, error) {
	ref, err := r.repo.Head()
	if err != nil {
		return "", fmt.Errorf("head: %w", err)
	}
	return ref.Hash().String(), nil
}
