// Package githubapi wraps go-github with the narrow operations this service needs:
// reading recent commits, opening/labeling PRs, finding the open PR for a branch,
// counting attempts, and reading the agent verify check. Deterministic tooling — no
// agent imports.
package githubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v78/github"

	"automation-agent/internal/auth"
)

// httpTimeout bounds every GitHub request. Without it the client relies solely on the
// caller's context, and a stalled connection could otherwise hang a long-running poll.
const httpTimeout = 30 * time.Second

// Client is a thin wrapper over *github.Client. Owner/repo are passed per call so
// one client serves many repositories.
type Client struct {
	gh *github.Client
	// authoredLogin is the GitHub login this client authors content as ("<slug>[bot]" in App
	// mode, the user login in PAT mode), resolved by the caller. When known, UpsertMarkerComment
	// edits only comments by this login — the authoritative ownership signal. Empty when identity
	// could not be resolved, in which case appAuthored selects a safe fallback.
	authoredLogin string
	// appAuthored is true when the REST token comes from a GitHub App installation, so this
	// client's own issue comments are authored by a bot user (type "Bot"). It is the fallback
	// ownership signal when authoredLogin is unknown: an in-place edit is then restricted to
	// bot-authored comments rather than any comment echoing the marker (GitHub rejects editing a
	// comment the client did not author). PAT/anonymous mode posts as a human, where this signal
	// does not apply.
	appAuthored bool
}

// Option configures a Client at construction.
type Option func(*Client)

// WithAuthoredLogin sets the login this client authors content as, so UpsertMarkerComment can edit
// only the comments it actually posted. An empty login leaves ownership to the author-type
// fallback (see ownsComment).
func WithAuthoredLogin(login string) Option {
	return func(c *Client) { c.authoredLogin = login }
}

// New builds a Client whose every REST request is authenticated by a fresh token
// from the provider (a static PAT, or an auto-refreshed App installation token).
// A StaticProvider holding an empty token yields an unauthenticated client (fine
// for public reads and tests).
func New(provider auth.TokenProvider, opts ...Option) *Client {
	gh := github.NewClient(&http.Client{
		Timeout:   httpTimeout,
		Transport: auth.NewRoundTripper(nil, provider),
	})
	// An App installation token posts comments as the app's bot user; a PAT posts as a human.
	// This distinguishes the two for the marker-upsert ownership fallback (see ownsComment).
	_, appAuthored := provider.(*auth.AppProvider)
	c := &Client{gh: gh, appAuthored: appAuthored}
	for _, opt := range opts {
		opt(c)
	}
	return c
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

// ReviewComment is one inline review comment on the head (RIGHT) side of a file. GitHub rejects
// an inline comment whose line is outside the PR's diff hunks, so the caller posts only in-diff
// findings here and lists the rest in the summary comment.
type ReviewComment struct {
	Path string
	Line int
	Side string // "RIGHT" (head side)
	Body string
}

// ReviewInput is an advisory pull-request review: a body plus optional inline comments. The
// reviewer never approves or requests changes (spec Decision 15), so the event is always COMMENT.
type ReviewInput struct {
	Body     string
	Comments []ReviewComment
}

// CreateReview posts an advisory (COMMENT) pull-request review with optional inline comments.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, in ReviewInput) error {
	comments := make([]*github.DraftReviewComment, 0, len(in.Comments))
	for _, rc := range in.Comments {
		comments = append(comments, &github.DraftReviewComment{
			Path: ptr(rc.Path), Body: ptr(rc.Body), Line: ptr(rc.Line), Side: ptr(rc.Side),
		})
	}
	req := &github.PullRequestReviewRequest{Event: ptr("COMMENT"), Comments: comments}
	if in.Body != "" {
		req.Body = ptr(in.Body)
	}
	if _, _, err := c.gh.PullRequests.CreateReview(ctx, owner, repo, number, req); err != nil {
		return fmt.Errorf("create review %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// UpsertMarkerComment edits the single issue comment this client authored whose body contains
// marker, or creates one if none exists. The reviewer's summary comment carries a hidden marker
// (spec Decision 9) so a re-review updates it in place instead of piling up duplicates. Only a
// comment the client could have authored is edited (see ownsComment): GitHub rejects editing a
// foreign comment, so a comment that merely echoes the marker must not hijack the upsert.
func (c *Client) UpsertMarkerComment(ctx context.Context, owner, repo string, number int, marker, body string) error {
	// An empty marker would match every comment (Contains("", …) is always true) and edit an
	// unrelated one; a body without the marker could never be found again, piling up duplicates.
	// Both are caller bugs, so fail fast rather than corrupt the PR's comments.
	if marker == "" {
		return fmt.Errorf("upsert comment %s/%s#%d: empty marker", owner, repo, number)
	}
	if !strings.Contains(body, marker) {
		return fmt.Errorf("upsert comment %s/%s#%d: body must contain the marker", owner, repo, number)
	}
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return fmt.Errorf("list comments %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, ic := range comments {
			if !strings.Contains(ic.GetBody(), marker) || !c.ownsComment(ic) {
				continue
			}
			if _, _, err := c.gh.Issues.EditComment(ctx, owner, repo, ic.GetID(), &github.IssueComment{Body: ptr(body)}); err != nil {
				// With a known login the match is authoritative, so any edit failure is a real
				// error. On the weak author-type fallback (identity unresolved) the match can be a
				// foreign bot that merely echoes the marker; a 403/404 there means "not ours", so
				// skip it and fall through to create rather than fail the whole publish.
				if c.authoredLogin == "" && isHTTPStatus(err, http.StatusForbidden, http.StatusNotFound) {
					continue
				}
				return fmt.Errorf("edit comment %s/%s#%d: %w", owner, repo, number, err)
			}
			return nil
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{Body: ptr(body)}); err != nil {
		return fmt.Errorf("create comment %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// ownsComment reports whether this client authored ic — the precondition for editing it in place
// (GitHub rejects editing a comment the client did not author). When the client's own login is
// known it is the authoritative check: only a byte-for-byte login match is owned, so a foreign
// comment — even another bot's — that merely echoes the marker is skipped and a fresh comment is
// created instead. When the login is unknown (identity could not be resolved), it falls back to
// author type: App mode trusts only bot-authored comments; PAT/anonymous (local-dev) trusts the
// marker alone, since there is no distinct identity to match against.
func (c *Client) ownsComment(ic *github.IssueComment) bool {
	if c.authoredLogin != "" {
		return ic.GetUser().GetLogin() == c.authoredLogin
	}
	if c.appAuthored {
		return ic.GetUser().GetType() == "Bot"
	}
	return true
}

// isHTTPStatus reports whether err is (or wraps) a GitHub API error with one of the given HTTP
// status codes.
func isHTTPStatus(err error, codes ...int) bool {
	var ge *github.ErrorResponse
	if !errors.As(err, &ge) || ge.Response == nil {
		return false
	}
	for _, code := range codes {
		if ge.Response.StatusCode == code {
			return true
		}
	}
	return false
}

// CheckRunInput describes the advisory agent-review check run (spec Decision 15): always
// completed, conclusion success or neutral — never failure, so it informs without gating merges.
type CheckRunInput struct {
	Name       string
	HeadSHA    string
	Conclusion string // "success" | "neutral"
	Title      string
	Summary    string
}

// CreateCheckRun posts a completed, advisory check run for the head SHA.
func (c *Client) CreateCheckRun(ctx context.Context, owner, repo string, in CheckRunInput) error {
	// The agent-review check is advisory and must never gate a merge (spec Decision 15), so the
	// conclusion is constrained here at the API boundary — a "failure"/"cancelled" can't slip in.
	if in.Conclusion != "success" && in.Conclusion != "neutral" {
		return fmt.Errorf("create check run %s/%s: advisory conclusion must be success or neutral, got %q", owner, repo, in.Conclusion)
	}
	_, _, err := c.gh.Checks.CreateCheckRun(ctx, owner, repo, github.CreateCheckRunOptions{
		Name:       in.Name,
		HeadSHA:    in.HeadSHA,
		Status:     ptr("completed"),
		Conclusion: ptr(in.Conclusion),
		Output:     &github.CheckRunOutput{Title: ptr(in.Title), Summary: ptr(in.Summary)},
	})
	if err != nil {
		return fmt.Errorf("create check run %s/%s @%s: %w", owner, repo, in.HeadSHA, err)
	}
	return nil
}

// ReviewCommentRef identifies an existing inline review comment for reconciliation: its GraphQL
// node id (the minimizeComment subject) and its body (which carries the hidden fingerprint marker).
type ReviewCommentRef struct {
	NodeID string
	Body   string
}

// ListReviewComments returns the PR's inline review comments (paginated). Reconciliation parses the
// fingerprint marker from each body to decide what to keep, add, or minimize.
func (c *Client) ListReviewComments(ctx context.Context, owner, repo string, number int) ([]ReviewCommentRef, error) {
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	var out []ReviewCommentRef
	for {
		comments, resp, err := c.gh.PullRequests.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list review comments %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, rc := range comments {
			out = append(out, ReviewCommentRef{NodeID: rc.GetNodeID(), Body: rc.GetBody()})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// MinimizeComment collapses a comment as OUTDATED via GraphQL (the REST API has no equivalent), so
// a finding that no longer applies is hidden rather than deleted — the thread is preserved.
// subjectID is the comment's GraphQL node id (ReviewCommentRef.NodeID).
func (c *Client) MinimizeComment(ctx context.Context, subjectID string) error {
	const mutation = `mutation($id:ID!){minimizeComment(input:{subjectId:$id,classifier:OUTDATED}){minimizedComment{isMinimized}}}`
	return c.graphql(ctx, mutation, map[string]any{"id": subjectID})
}

// graphql POSTs a GraphQL operation to the GraphQL endpoint over the same authenticated HTTP client
// as REST (the installation token authenticates both). The endpoint is derived from the REST
// BaseURL so a test can point it at an httptest stub. A non-2xx status or a non-empty GraphQL
// errors array becomes a Go error.
func (c *Client) graphql(ctx context.Context, query string, variables map[string]any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("graphql: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("graphql: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.gh.Client().Do(req)
	if err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("graphql: unexpected status %s", resp.Status)
	}
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("graphql: decode response: %w", err)
	}
	if len(body.Errors) > 0 {
		return fmt.Errorf("graphql: %s", body.Errors[0].Message)
	}
	return nil
}

// graphqlURL derives the GraphQL endpoint from the REST BaseURL. api.github.com's REST base is
// "https://api.github.com/" and its GraphQL endpoint is "https://api.github.com/graphql"; a test
// stub's BaseURL yields that server's /graphql. GitHub Enterprise Server is the exception: it
// serves REST at "<host>/api/v3" but GraphQL at "<host>/api/graphql", so map that path explicitly.
func (c *Client) graphqlURL() string {
	base := strings.TrimSuffix(c.gh.BaseURL.String(), "/")
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	}
	return base + "/graphql"
}

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

// PullRequestHeadSHA returns the PR's current head commit SHA. The reviewer compares it to the
// SHA carried by a review task to detect a task superseded by a newer push (coalesce-to-latest)
// and skip a stale review.
func (c *Client) PullRequestHeadSHA(ctx context.Context, owner, repo string, number int) (string, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("get PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return pr.GetHead().GetSHA(), nil
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

// PRFile is one changed file in a pull request: its path, change status, line counts, and
// the unified diff patch. Patch carries the hunk text the reviewer needs to map a finding to
// a diff line; GitHub omits it for binary or very large files, so it is then empty — kept, not
// an error. An empty Patch is ambiguous (binary vs. oversized text), so size accounting must
// not treat it as zero diff bytes: Additions+Deletions are reported even when the patch is
// omitted, letting an omitted text diff be charged conservatively from its line counts.
type PRFile struct {
	Path         string
	PreviousPath string // prior path for a rename, else empty
	Status       string // added | modified | removed | renamed | copied | changed
	Additions    int
	Deletions    int
	Patch        string // unified diff hunks; empty for binary/oversized files
}

// ListPRFiles returns every changed file in a pull request, following pagination. It is the
// reviewer's primary input (changed files + patches), fetched via REST.
func (c *Client) ListPRFiles(ctx context.Context, owner, repo string, number int) ([]PRFile, error) {
	opts := &github.ListOptions{PerPage: 100}
	var out []PRFile
	for {
		files, resp, err := c.gh.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list PR files %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, f := range files {
			out = append(out, PRFile{
				Path:         f.GetFilename(),
				PreviousPath: f.GetPreviousFilename(),
				Status:       f.GetStatus(),
				Additions:    f.GetAdditions(),
				Deletions:    f.GetDeletions(),
				Patch:        f.GetPatch(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
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

// PullRequestEvent is the parsed essentials of a GitHub pull_request webhook event — the
// reviewer's native-event kickoff. The diff itself is fetched separately via ListPRFiles
// (the event body carries only metadata).
type PullRequestEvent struct {
	Action       string // opened | reopened | synchronize | ready_for_review | ...
	Number       int
	RepoFullName string // owner/name
	HeadRef      string // source branch
	HeadSHA      string
	BaseRef      string // target branch
	Draft        bool
	Labels       []string
	AuthorLogin  string // PR author login (e.g. "dependabot[bot]")
}

// ParsePullRequestEvent parses a pull_request webhook body into the fields the reviewer
// gates on. It mirrors ParseCheckRunEvent: the webhook JSON is decoded in the tooling layer
// so the agent consumes a stable projection, never the raw SDK type.
func ParsePullRequestEvent(b []byte) (PullRequestEvent, error) {
	var ev github.PullRequestEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		return PullRequestEvent{}, fmt.Errorf("parse pull_request event: %w", err)
	}
	pr := ev.GetPullRequest()
	out := PullRequestEvent{
		Action:       ev.GetAction(),
		Number:       pr.GetNumber(),
		RepoFullName: ev.GetRepo().GetFullName(),
		HeadRef:      pr.GetHead().GetRef(),
		HeadSHA:      pr.GetHead().GetSHA(),
		BaseRef:      pr.GetBase().GetRef(),
		Draft:        pr.GetDraft(),
		AuthorLogin:  pr.GetUser().GetLogin(),
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.GetName())
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
