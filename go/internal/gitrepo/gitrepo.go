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

// Repo is a cloned working tree.
type Repo struct {
	repo *git.Repository
	wt   *git.Worktree
	dir  string
	auth transport.AuthMethod
	now  func() time.Time
}

// Auth carries the credentials Clone/Push use. Which one applies is chosen by the
// clone URL scheme, not by the caller: an https remote uses Token (GitHub x-access-token
// basic auth), an ssh remote (git@… / ssh://…) uses SSHKey or the ssh-agent.
type Auth struct {
	// Token is the GitHub token used as x-access-token basic auth on https remotes.
	// Empty means anonymous (public read only). Ignored for ssh remotes.
	Token string
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
// x-access-token basic auth (or nil/anonymous) for an https remote. Host-key checking
// stays on for ssh — go-git defaults HostKeyCallback to the user's known_hosts.
func authFor(url string, a Auth) (transport.AuthMethod, error) {
	if isSSHURL(url) {
		return sshAuth(a.SSHKey)
	}
	if a.Token == "" {
		return nil, nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: a.Token}, nil
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
	am, err := authFor(url, auth)
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
	return &Repo{repo: repo, wt: wt, dir: dir, auth: am, now: time.Now}, nil
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
func (r *Repo) Push(ctx context.Context) error {
	err := r.repo.PushContext(ctx, &git.PushOptions{Auth: r.auth})
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
