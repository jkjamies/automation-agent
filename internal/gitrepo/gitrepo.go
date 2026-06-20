// Package gitrepo wraps go-git for the working-tree operations the lint-fixer
// needs: clone, branch, stage-all, commit, push. Pure-Go (no git binary).
// Deterministic tooling — no agent imports.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
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

// authFor builds GitHub token auth (x-access-token) or nil for local/anonymous.
func authFor(token string) transport.AuthMethod {
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}
}

// Clone clones url into dir (which must not already exist). A non-empty token is
// used as GitHub HTTP auth.
func Clone(ctx context.Context, url, dir, token string) (*Repo, error) {
	auth := authFor(token)
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: url, Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", url, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	return &Repo{repo: repo, wt: wt, dir: dir, auth: auth, now: time.Now}, nil
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

// CommitAll stages every change (including deletions) and commits, returning the
// new commit SHA.
func (r *Repo) CommitAll(msg string, a Author) (string, error) {
	if err := r.wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", fmt.Errorf("stage changes: %w", err)
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
