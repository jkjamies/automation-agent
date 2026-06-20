// Package notify posts provider-agnostic messages to a chat destination (Slack or
// Microsoft Teams) behind a single interface, so the workflow choice is a config
// flag, not a code change. It is deterministic tooling — it must not import agents.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message is a provider-agnostic notification.
type Message struct {
	Title string // short bold heading
	Text  string // body
	Link  string // optional URL (e.g. a PR) rendered as an action/link
}

// Notifier posts messages to a chat destination.
type Notifier interface {
	Notify(ctx context.Context, m Message) error
}

// New returns a Notifier for the given provider ("slack" or "teams") using the
// matching webhook URL.
func New(provider, slackURL, teamsURL string) (Notifier, error) {
	switch provider {
	case "slack":
		if slackURL == "" {
			return nil, fmt.Errorf("SLACK_WEBHOOK_URL is required for notify provider slack")
		}
		return NewSlack(slackURL), nil
	case "teams":
		if teamsURL == "" {
			return nil, fmt.Errorf("TEAMS_WEBHOOK_URL is required for notify provider teams")
		}
		return NewTeams(teamsURL), nil
	default:
		return nil, fmt.Errorf("unknown notify provider %q (want slack|teams)", provider)
	}
}

// defaultClient is the HTTP client used by notifiers unless one is injected.
func defaultClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// postJSON marshals payload and POSTs it, returning an error on a non-2xx status.
func postJSON(ctx context.Context, httpc *http.Client, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("post notification: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("notification rejected: %s: %s", resp.Status, snippet)
	}
	return nil
}
