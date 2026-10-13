// Package jira is a small Jira Cloud REST v3 client: read an issue, comment on it, edit
// its labels, and transition it. It is deterministic tooling with no LLM involvement.
package jira

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

// Issue is the subset of a Jira issue the pipeline reasons about. Description and
// comment bodies are rendered from Atlassian Document Format to plain text.
type Issue struct {
	Key         string
	Project     string
	URL         string // human-facing browse link
	Type        string
	Status      string
	Summary     string
	Description string
	Labels      []string
	Components  []string
	Comments    []Comment
}

// Comment is one issue comment, rendered to plain text.
type Comment struct {
	Author string
	Body   string
}

// HasLabel reports whether the issue carries label (case-sensitive, as Jira is).
func (i Issue) HasLabel(label string) bool {
	for _, l := range i.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// Client talks to one Jira site.
type Client struct {
	base *url.URL
	http *http.Client
	auth func(*http.Request)
}

// NewClient builds a client for baseURL. With a non-empty email it uses Jira Cloud basic
// auth (email + API token); otherwise the token is sent as a bearer token (Data Center PATs).
// A nil hc uses a client with a 30 s timeout.
func NewClient(baseURL, email, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("jira: invalid base URL %q", baseURL)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{base: u, http: hc}
	if email != "" {
		c.auth = func(r *http.Request) { r.SetBasicAuth(email, token) }
	} else {
		c.auth = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	return c, nil
}

// BrowseURL is the human-facing link to an issue.
func (c *Client) BrowseURL(key string) string {
	return c.base.String() + "/browse/" + url.PathEscape(key)
}

type issueResponse struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Labels      []string        `json:"labels"`
		IssueType   struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Status struct {
			Name string `json:"name"`
		} `json:"status"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
		Comment struct {
			Comments []struct {
				Author struct {
					DisplayName string `json:"displayName"`
				} `json:"author"`
				Body json.RawMessage `json:"body"`
			} `json:"comments"`
		} `json:"comment"`
	} `json:"fields"`
}

// Issue fetches one issue by key.
func (c *Client) Issue(ctx context.Context, key string) (Issue, error) {
	q := url.Values{"fields": {"summary,description,labels,issuetype,status,project,components,comment"}}
	var r issueResponse
	if err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"?"+q.Encode(), nil, &r); err != nil {
		return Issue{}, err
	}
	is := Issue{
		Key:         r.Key,
		Project:     r.Fields.Project.Key,
		URL:         c.BrowseURL(r.Key),
		Type:        r.Fields.IssueType.Name,
		Status:      r.Fields.Status.Name,
		Summary:     r.Fields.Summary,
		Description: ADFText(r.Fields.Description),
		Labels:      r.Fields.Labels,
	}
	for _, comp := range r.Fields.Components {
		is.Components = append(is.Components, comp.Name)
	}
	for _, cm := range r.Fields.Comment.Comments {
		is.Comments = append(is.Comments, Comment{Author: cm.Author.DisplayName, Body: ADFText(cm.Body)})
	}
	return is, nil
}

// Comment adds a plain-text comment (converted to ADF; URLs become links).
func (c *Client) Comment(ctx context.Context, key, text string) error {
	body := map[string]any{"body": TextToADF(text)}
	return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", body, nil)
}

// EditLabels adds and removes labels in one update, leaving other labels untouched.
func (c *Client) EditLabels(ctx context.Context, key string, add, remove []string) error {
	var ops []map[string]string
	for _, l := range add {
		ops = append(ops, map[string]string{"add": l})
	}
	for _, l := range remove {
		ops = append(ops, map[string]string{"remove": l})
	}
	if len(ops) == 0 {
		return nil
	}
	body := map[string]any{"update": map[string]any{"labels": ops}}
	return c.do(ctx, http.MethodPut, "/rest/api/3/issue/"+url.PathEscape(key), body, nil)
}

// ErrNoTransition is returned when no available transition matches the requested name.
var ErrNoTransition = errors.New("jira: no matching transition")

// Transition moves the issue through the workflow transition whose name — or whose
// target status name — matches name (case-insensitive). Workflows differ per project,
// so matching either keeps configuration portable.
func (c *Client) Transition(ctx context.Context, key, name string) error {
	var list struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/transitions"
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return err
	}
	for _, t := range list.Transitions {
		if strings.EqualFold(t.Name, name) || strings.EqualFold(t.To.Name, name) {
			body := map[string]any{"transition": map[string]string{"id": t.ID}}
			return c.do(ctx, http.MethodPost, path, body, nil)
		}
	}
	return fmt.Errorf("%w %q on %s", ErrNoTransition, name, key)
}

// maxErrorBody caps how much of an error response is quoted back in an error.
const maxErrorBody = 2 << 10

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("jira: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return fmt.Errorf("jira: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("jira: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("jira: decode %s %s: %w", method, path, err)
	}
	return nil
}
