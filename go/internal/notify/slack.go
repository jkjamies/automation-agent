package notify

import (
	"context"
	"net/http"
	"strings"
)

// slackNotifier posts to a Slack incoming webhook. The minimal accepted payload
// is {"text": "..."}.
type slackNotifier struct {
	url   string
	httpc *http.Client
}

// NewSlack builds a Slack notifier for the given incoming-webhook URL.
func NewSlack(url string) Notifier {
	return &slackNotifier{url: url, httpc: defaultClient()}
}

func (s *slackNotifier) Notify(ctx context.Context, m Message) error {
	return postJSON(ctx, s.httpc, s.url, map[string]string{"text": slackText(m)})
}

// slackText renders a Message as Slack mrkdwn.
func slackText(m Message) string {
	var b strings.Builder
	if m.Title != "" {
		b.WriteString("*")
		b.WriteString(m.Title)
		b.WriteString("*")
	}
	if m.Text != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.Text)
	}
	if m.Link != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("<")
		b.WriteString(m.Link)
		b.WriteString(">")
	}
	return b.String()
}
