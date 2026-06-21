// Package githubapi wraps go-github with the narrow operations this service needs:
// reading recent commits, opening/labeling/finding agent PRs, counting attempts,
// and reading the agent verify check. Deterministic tooling — no agent imports.
package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/go-github/v78/github"
)

// Client is a thin wrapper over *github.Client. Owner/repo are passed per call so
// one client serves many repositories.
type Client struct {
	gh *github.Client
}

// New builds a Client. An empty token yields an unauthenticated client (fine for
// public reads and tests).
func New(token string) *Client {
	gh := github.NewClient(nil)
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

// FindAgentPRs lists open PRs carrying the given label.
func (c *Client) FindAgentPRs(ctx context.Context, owner, repo, label string) ([]PR, error) {
	opts := &github.PullRequestListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}}
	var out []PR
	for {
		prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list PRs %s/%s: %w", owner, repo, err)
		}
		for _, pr := range prs {
			p := toPR(pr)
			if hasLabel(p.Labels, label) {
				out = append(out, p)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// AgentCheck returns the named check's state for ref, or {Found:false} if absent.
func (c *Client) AgentCheck(ctx context.Context, owner, repo, ref, checkName string) (CheckResult, error) {
	res, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, &github.ListCheckRunsOptions{
		CheckName: ptr(checkName),
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

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
