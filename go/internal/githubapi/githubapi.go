// Package githubapi wraps go-github with the narrow operations this service needs:
// reading recent commits, opening/labeling PRs, finding the open PR for a branch,
// counting attempts, and reading the agent verify check. Deterministic tooling — no
// agent imports.
package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v78/github"
)

// httpTimeout bounds every GitHub request. Without it the client relies solely on the
// caller's context, and a stalled connection could otherwise hang a long-running poll.
const httpTimeout = 30 * time.Second

// Client is a thin wrapper over *github.Client. Owner/repo are passed per call so
// one client serves many repositories.
type Client struct {
	gh *github.Client
}

// New builds a Client. An empty token yields an unauthenticated client (fine for
// public reads and tests).
func New(token string) *Client {
	gh := github.NewClient(&http.Client{Timeout: httpTimeout})
	if token != "" {
		gh = gh.WithAuthToken(token)
	}
	return &Client{gh: gh}
}

// Commit is a minimal commit projection for digests.
type Commit struct {
	SHA     string
	Message string
	Author  string
	URL     string
	When    time.Time
}

// PR is a minimal pull-request projection.
type PR struct {
	Number  int
	Title   string
	Branch  string
	HeadSHA string
	URL     string
	Labels  []string
}

// PRInput describes a pull request to open.
type PRInput struct {
	Title string
	Head  string // source branch
	Base  string // target branch
	Body  string
}

// CheckResult is the agent verify check's state for a ref.
type CheckResult struct {
	Found       bool
	Name        string
	Status      string // queued | in_progress | completed
	Conclusion  string // success | failure | ... (when completed)
	OutputText  string // the check's output (lint findings), used to re-triage on resume
	StartedAt   time.Time
	CompletedAt time.Time
}

func ptr[T any](v T) *T { return &v }

// Comparison summarizes what changed between two refs (base...head).
type Comparison struct {
	TotalCommits int
	Files        []ChangedFile
}

// ChangedFile is one file in a comparison.
type ChangedFile struct {
	Path      string
	Status    string // added | modified | removed | renamed | ...
	Additions int
	Deletions int
}

// Compare returns the commits and files changed between base and head (base...head). It is
// how a terminal summary reports what the agent actually did across its attempts, since the
// per-attempt work product lives only in the PR, not the session.
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string) (Comparison, error) {
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return Comparison{}, fmt.Errorf("compare %s/%s %s...%s: %w", owner, repo, base, head, err)
	}
	out := Comparison{TotalCommits: cmp.GetTotalCommits()}
	for _, f := range cmp.Files {
		out.Files = append(out.Files, ChangedFile{
			Path: f.GetFilename(), Status: f.GetStatus(),
			Additions: f.GetAdditions(), Deletions: f.GetDeletions(),
		})
	}
	return out, nil
}

// ListCommitsSince returns commits to owner/repo authored since the given time.
func (c *Client) ListCommitsSince(ctx context.Context, owner, repo string, since time.Time) ([]Commit, error) {
	opts := &github.CommitsListOptions{Since: since, ListOptions: github.ListOptions{PerPage: 100}}
	var out []Commit
	for {
		commits, resp, err := c.gh.Repositories.ListCommits(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list commits %s/%s: %w", owner, repo, err)
		}
		for _, rc := range commits {
			out = append(out, toCommit(rc))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// CreatePR opens a pull request.
func (c *Client) CreatePR(ctx context.Context, owner, repo string, in PRInput) (PR, error) {
	pr, _, err := c.gh.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: ptr(in.Title), Head: ptr(in.Head), Base: ptr(in.Base), Body: ptr(in.Body),
	})
	if err != nil {
		return PR{}, fmt.Errorf("create PR %s/%s: %w", owner, repo, err)
	}
	return toPR(pr), nil
}

// AddLabels adds labels to a PR (PRs are issues for the labels API).
func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error {
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, labels)
	if err != nil {
		return fmt.Errorf("add labels to %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// FindOpenPRByBranch returns the open PR whose head is the given branch, if one exists.
// Lookup is by branch (the GitHub `head=owner:branch` filter), not the agent label — the
// label is write-only, applied on creation for humans to filter on.
func (c *Client) FindOpenPRByBranch(ctx context.Context, owner, repo, branch string) (PR, bool, error) {
	prs, _, err := c.gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open", Head: owner + ":" + branch, ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return PR{}, false, fmt.Errorf("list PRs %s/%s head %s: %w", owner, repo, branch, err)
	}
	if len(prs) == 0 {
		return PR{}, false, nil
	}
	return toPR(prs[0]), true, nil
}

// AgentCheck returns the named check's state for ref, or {Found:false} if absent.
func (c *Client) AgentCheck(ctx context.Context, owner, repo, ref, checkName string) (CheckResult, error) {
	res, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, &github.ListCheckRunsOptions{
		CheckName: ptr(checkName),
		Filter:    ptr("latest"), // on a re-run, return only the most recent run per check
	})
	if err != nil {
		return CheckResult{}, fmt.Errorf("list check runs %s/%s@%s: %w", owner, repo, ref, err)
	}
	if res.GetTotal() == 0 || len(res.CheckRuns) == 0 {
		return CheckResult{Found: false}, nil
	}
	cr := res.CheckRuns[0]
	out := CheckResult{
		Found:       true,
		Name:        cr.GetName(),
		Status:      cr.GetStatus(),
		Conclusion:  cr.GetConclusion(),
		StartedAt:   cr.GetStartedAt().Time,
		CompletedAt: cr.GetCompletedAt().Time,
	}
	if o := cr.GetOutput(); o != nil {
		out.OutputText = o.GetText()
		if out.OutputText == "" {
			out.OutputText = o.GetSummary()
		}
	}
	return out, nil
}

// GetFileContent returns the decoded contents of a file at ref (ref may be "" for
// the default branch).
func (c *Client) GetFileContent(ctx context.Context, owner, repo, path, ref string) (string, error) {
	fc, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return "", fmt.Errorf("get %s/%s:%s: %w", owner, repo, path, err)
	}
	if fc == nil {
		return "", fmt.Errorf("%s is a directory, not a file", path)
	}
	content, err := fc.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	return content, nil
}

// CheckEvent is the parsed essentials of a GitHub check_run webhook event.
type CheckEvent struct {
	Action       string // created | completed | rerequested
	CheckName    string
	Status       string // queued | in_progress | completed
	Conclusion   string // success | failure | ... (when completed)
	HeadSHA      string
	PRNumber     int
	PRBranch     string
	RepoFullName string // owner/name
	OutputText   string // the check's output (lint findings), used to re-triage on resume
}

// ParseCheckRunEvent parses a check_run webhook body.
func ParseCheckRunEvent(b []byte) (CheckEvent, error) {
	var ev github.CheckRunEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return CheckEvent{}, fmt.Errorf("parse check_run event: %w", err)
	}
	cr := ev.GetCheckRun()
	out := CheckEvent{
		Action:       ev.GetAction(),
		CheckName:    cr.GetName(),
		Status:       cr.GetStatus(),
		Conclusion:   cr.GetConclusion(),
		HeadSHA:      cr.GetHeadSHA(),
		RepoFullName: ev.GetRepo().GetFullName(),
	}
	if prs := cr.PullRequests; len(prs) > 0 {
		out.PRNumber = prs[0].GetNumber()
		out.PRBranch = prs[0].GetHead().GetRef()
	}
	if o := cr.GetOutput(); o != nil {
		out.OutputText = o.GetText()
		if out.OutputText == "" {
			out.OutputText = o.GetSummary()
		}
	}
	return out, nil
}

func toCommit(rc *github.RepositoryCommit) Commit {
	c := rc.GetCommit()
	return Commit{
		SHA:     rc.GetSHA(),
		Message: c.GetMessage(),
		Author:  c.GetAuthor().GetName(),
		URL:     rc.GetHTMLURL(),
		When:    c.GetAuthor().GetDate().Time,
	}
}

func toPR(pr *github.PullRequest) PR {
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.GetName())
	}
	return PR{
		Number:  pr.GetNumber(),
		Title:   pr.GetTitle(),
		Branch:  pr.GetHead().GetRef(),
		HeadSHA: pr.GetHead().GetSHA(),
		URL:     pr.GetHTMLURL(),
		Labels:  labels,
	}
}
