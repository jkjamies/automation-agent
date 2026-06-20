package fixflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jkjamies/automation-agent/internal/githubapi"
	"github.com/jkjamies/automation-agent/internal/gitrepo"
)

// GitHub is the slice of githubapi the apply step needs (consumer-defined, fakeable).
type GitHub interface {
	FindAgentPRs(ctx context.Context, owner, repo, label string) ([]githubapi.PR, error)
	CreatePR(ctx context.Context, owner, repo string, in githubapi.PRInput) (githubapi.PR, error)
	AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error
}

// FileEdit is a whole-file write an analyze step produces (a rewritten source file,
// a generated test file, …).
type FileEdit struct {
	Path    string // repo-relative path
	Content string
}

// ApplyConfig parameterizes one apply.
type ApplyConfig struct {
	Owner, Repo   string
	CloneURL      string
	Token         string
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
	repo, err := gitrepo.Clone(ctx, cfg.CloneURL, dir, cfg.Token)
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
		return ApplyResult{}, err
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
		full := repo.Path(e.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", e.Path, err)
		}
		if err := os.WriteFile(full, []byte(e.Content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", e.Path, err)
		}
	}
	return nil
}

// ensurePR returns the existing agent PR for the branch, or creates and labels one.
func ensurePR(ctx context.Context, gh GitHub, cfg ApplyConfig) (githubapi.PR, error) {
	existing, err := gh.FindAgentPRs(ctx, cfg.Owner, cfg.Repo, cfg.Label)
	if err != nil {
		return githubapi.PR{}, err
	}
	for _, pr := range existing {
		if pr.Branch == cfg.Branch {
			return pr, nil
		}
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
