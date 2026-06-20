// Package lintfixer implements the autonomous lint-remediation workflow.
//
// This file holds the deterministic "apply one fix" mechanics, which are
// independent of the suspend/resume loop. The loop, the analyze LLM agent, and the
// resume wiring are added once the suspend/resume design notes are finalized.
package lintfixer

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

// FileEdit is a whole-file write the fix applies (the analyze step produces these).
type FileEdit struct {
	Path    string // repo-relative path
	Content string
}

// ApplyConfig parameterizes one fix attempt.
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

// ApplyFix clones the repo into a temp dir, applies edits on a fresh agent branch,
// commits, pushes, and ensures a labeled PR exists. It returns the PR and the new
// head SHA.
//
// With NewBranch=true it creates the agent branch from the clone's HEAD (kickoff);
// with NewBranch=false it checks out the existing remote branch and adds a commit
// onto the previous fix (retry).
func ApplyFix(ctx context.Context, gh GitHub, cfg ApplyConfig, edits []FileEdit) (ApplyResult, error) {
	if len(edits) == 0 {
		return ApplyResult{}, fmt.Errorf("apply fix: no edits to apply")
	}

	dir, err := os.MkdirTemp("", "agentfix-*")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(dir)

	repo, err := gitrepo.Clone(ctx, cfg.CloneURL, dir, cfg.Token)
	if err != nil {
		return ApplyResult{}, err
	}
	if cfg.NewBranch {
		if err := repo.Checkout(cfg.Branch, true); err != nil {
			return ApplyResult{}, err
		}
	} else {
		if err := repo.CheckoutRemote(cfg.Branch); err != nil {
			return ApplyResult{}, err
		}
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
// On a retry the branch's push already updated the existing PR, so we don't reopen.
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
