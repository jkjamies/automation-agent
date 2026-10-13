package jira

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Event is the part of a Jira webhook delivery the trigger decision needs. The issue's
// content is deliberately not carried: the pipeline re-reads the issue through the API,
// so a delivery can name a ticket but never supply its text.
type Event struct {
	Name     string   // webhookEvent, e.g. "jira:issue_updated"
	IssueKey string   // e.g. "PROJ-123"
	Labels   []string // labels on the issue at delivery time
	// LabelsChanged is true when the delivery's changelog touched the labels field,
	// which is how an "agent-ready" label being added shows up.
	LabelsChanged bool
}

// ErrNotIssueEvent marks a delivery that carries no issue (e.g. a project or sprint event).
var ErrNotIssueEvent = errors.New("jira: webhook delivery has no issue")

// ParseEvent decodes a Jira webhook body (an admin webhook, or an Automation
// "Send web request" action using the {{issue}} smart-value payload).
func ParseEvent(body []byte) (Event, error) {
	var p struct {
		WebhookEvent string `json:"webhookEvent"`
		Issue        *struct {
			Key    string `json:"key"`
			Fields struct {
				Labels []string `json:"labels"`
			} `json:"fields"`
		} `json:"issue"`
		Changelog struct {
			Items []struct {
				Field string `json:"field"`
			} `json:"items"`
		} `json:"changelog"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, err
	}
	if p.Issue == nil || p.Issue.Key == "" {
		return Event{}, ErrNotIssueEvent
	}
	ev := Event{Name: p.WebhookEvent, IssueKey: p.Issue.Key, Labels: p.Issue.Fields.Labels}
	for _, it := range p.Changelog.Items {
		if strings.EqualFold(it.Field, "labels") {
			ev.LabelsChanged = true
		}
	}
	return ev, nil
}

// TokenHeader carries a shared secret for senders that cannot sign, such as a Jira
// Automation "Send web request" action with a custom header.
const TokenHeader = "X-Ticketflow-Token"

// Authentic reports whether a delivery was sent by someone holding secret. A signed
// delivery ("X-Hub-Signature: sha256=<hex HMAC of the body>", as Jira Cloud sends for a
// webhook configured with a secret) is checked by HMAC; otherwise TokenHeader must equal
// the secret. Both comparisons are constant-time. An empty secret authenticates nothing.
func Authentic(secret string, h http.Header, body []byte) bool {
	if secret == "" {
		return false
	}
	if sig := h.Get("X-Hub-Signature"); sig != "" {
		algo, digest, ok := strings.Cut(sig, "=")
		if !ok || !strings.EqualFold(algo, "sha256") {
			return false
		}
		got, err := hex.DecodeString(digest)
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return hmac.Equal(got, mac.Sum(nil))
	}
	tok := h.Get(TokenHeader)
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1
}
