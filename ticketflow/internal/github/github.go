// Package github is the small slice of the GitHub REST API the pipeline needs: read a
// repository's default branch, open (or find) a pull request, and label it.
//
// Merging is deliberately absent. Nothing in ticketflow can merge; the human merge gate is
// enforced by branch protection, and this client not having the call is the first layer.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a token-authenticated REST client.
type Client struct {
	api   string
	token string
	http  *http.Client
}

// NewClient builds a client for the REST base api (https://api.github.com, or
// https://HOST/api/v3 on GitHub Enterprise Server). A nil hc uses a 30 s timeout.
func NewClient(api, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{api: strings.TrimRight(api, "/"), token: token, http: hc}
}

// NewPR describes a pull request to open.
type NewPR struct {
	Title string
	Head  string // branch name in the same repository
	Base  string
	Body  string
	Draft bool
}

// PR is an opened pull request.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

// DefaultBranch returns repo's default branch. repo is "owner/name".
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// OpenPR opens a pull request, or returns the open one already proposing the same head,
// so a retried run converges on one PR instead of failing.
func (c *Client) OpenPR(ctx context.Context, repo string, pr NewPR) (PR, error) {
	body := map[string]any{"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body, "draft": pr.Draft}
	var out PR
	err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls", body, &out)
	if err == nil {
		return out, nil
	}
	var se *statusError
	if !errors.As(err, &se) || se.code != http.StatusUnprocessableEntity {
		return PR{}, err
	}
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"head": {owner + ":" + pr.Head}, "state": {"open"}}
	var existing []PR
	if lerr := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &existing); lerr != nil || len(existing) == 0 {
		return PR{}, err
	}
	return existing[0], nil
}

// AddLabels adds labels to an issue or pull request.
func (c *Client) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number), map[string]any{"labels": labels}, nil)
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

const maxErrorBody = 2 << 10

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, body)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &statusError{code: resp.StatusCode, msg: fmt.Sprintf("github: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}
