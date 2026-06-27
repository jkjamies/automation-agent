package fixflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation-agent/internal/githubapi"
	"automation-agent/internal/gitrepo"
)

// GitHub is the slice of githubapi the apply step needs (consumer-defined, fakeable).
type GitHub interface {
	FindOpenPRByBranch(ctx context.Context, owner, repo, branch string) (githubapi.PR, bool, error)
	CreatePR(ctx context.Context, owner, repo string, in githubapi.PRInput) (githubapi.PR, error)
	AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error
	Compare(ctx context.Context, owner, repo, base, head string) (githubapi.Comparison, error)
}

// FileEdit is a whole-file write an analyze step produces (a rewritten source file,
// a generated test file, …).
type FileEdit struct {
	Path    string // repo-relative path
	Content string
}

// ApplyConfig parameterizes one apply.
type ApplyConfig struct {
	Owner, Repo string
	CloneURL    string
	// Provider yields the GitHub token for https git transport, fetched fresh per op
	// (scoped to Owner/Repo). Nil/empty token means anonymous. Ignored for an ssh CloneURL.
	Provider gitrepo.TokenProvider
	// SSHKey is the explicit private-key path for an ssh CloneURL (GIT_SSH_KEY); empty
	// falls back to ssh-agent then default identities. Ignored for an https CloneURL.
	SSHKey        string
	Base          string // base branch the PR targets
	Branch        string // agent working branch
	NewBranch     bool   // true on kickoff (create from base); false on retry (reuse remote branch)
	Label         string
	CommitMessage string
	PRTitle       string
	PRBody        string
	Author        gitrepo.Author
}

// ApplyResult is the outcome of one apply.
type ApplyResult struct {
	PR      githubapi.PR
	HeadSHA string
}

// Open clones the repo into a fresh temp dir and checks out the agent branch — the
// single checkout the explorer reads, the executor writes into, and the commit step
// pushes. NewBranch=true creates the branch from HEAD (kickoff); false checks out the
// existing remote branch (retry). The caller must os.RemoveAll(repo.Dir()) when done.
func Open(ctx context.Context, cfg ApplyConfig) (*gitrepo.Repo, error) {
	dir, err := os.MkdirTemp("", "agentfix-*")
	if err != nil {
		return nil, fmt.Errorf("tempdir: %w", err)
	}
	repo, err := gitrepo.Clone(ctx, cfg.CloneURL, dir, gitrepo.Auth{
		Provider: cfg.Provider, Repo: cfg.Owner + "/" + cfg.Repo, SSHKey: cfg.SSHKey,
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if cfg.NewBranch {
		err = repo.Checkout(cfg.Branch, true)
	} else {
		err = repo.CheckoutRemote(cfg.Branch)
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return repo, nil
}

// Commit writes edits into the working tree, commits, pushes, and ensures a labeled
// PR exists.
func Commit(ctx context.Context, gh GitHub, repo *gitrepo.Repo, cfg ApplyConfig, edits []FileEdit) (ApplyResult, error) {
	if len(edits) == 0 {
		return ApplyResult{}, fmt.Errorf("apply: no edits to apply")
	}
	if err := writeEdits(repo, edits); err != nil {
		return ApplyResult{}, err
	}
	sha, err := repo.CommitAll(cfg.CommitMessage, cfg.Author)
	if err != nil {
		// A clean tree is a benign no-op, not a failure: the LLM re-emitted content that
		// already matches the branch (common on retry). Resolve it like a real apply —
		// reuse the current HEAD, push (an up-to-date push is fine), and ensure the PR —
		// so the run parks on CI instead of being reported to a human as a failed fix.
		if errors.Is(err, gitrepo.ErrNoChanges) {
			if sha, err = repo.Head(); err != nil {
				return ApplyResult{}, err
			}
		} else {
			return ApplyResult{}, err
		}
	}
	if err := repo.Push(ctx); err != nil {
		return ApplyResult{}, err
	}
	pr, err := ensurePR(ctx, gh, cfg)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{PR: pr, HeadSHA: sha}, nil
}

// ApplyFix opens a checkout and commits edits in one step (no analysis in between) —
// a convenience used in tests; the engine interleaves analysis between Open and Commit.
func ApplyFix(ctx context.Context, gh GitHub, cfg ApplyConfig, edits []FileEdit) (ApplyResult, error) {
	repo, err := Open(ctx, cfg)
	if err != nil {
		return ApplyResult{}, err
	}
	defer os.RemoveAll(repo.Dir())
	return Commit(ctx, gh, repo, cfg, edits)
}

func writeEdits(repo *gitrepo.Repo, edits []FileEdit) error {
	for _, e := range edits {
		// Reject LLM-controlled paths that escape the checkout (path traversal).
		full, err := safeJoin(repo.Dir(), e.Path)
		if err != nil {
			return fmt.Errorf("reject edit %q: %w", e.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", e.Path, err)
		}
		if err := os.WriteFile(full, []byte(e.Content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", e.Path, err)
		}
	}
	return nil
}

// ensurePR returns the existing open PR for the branch, or creates and labels one. The
// lookup is by head branch (not the agent label, which is write-only and never read back).
func ensurePR(ctx context.Context, gh GitHub, cfg ApplyConfig) (githubapi.PR, error) {
	existing, found, err := gh.FindOpenPRByBranch(ctx, cfg.Owner, cfg.Repo, cfg.Branch)
	if err != nil {
		return githubapi.PR{}, err
	}
	if found {
		return existing, nil
	}
	pr, err := gh.CreatePR(ctx, cfg.Owner, cfg.Repo, githubapi.PRInput{
		Title: cfg.PRTitle, Head: cfg.Branch, Base: cfg.Base, Body: cfg.PRBody,
	})
	if err != nil {
		return githubapi.PR{}, err
	}
	if err := gh.AddLabels(ctx, cfg.Owner, cfg.Repo, pr.Number, cfg.Label); err != nil {
		return githubapi.PR{}, err
	}
	pr.Labels = append(pr.Labels, cfg.Label)
	return pr, nil
}
