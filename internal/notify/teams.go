package notify

import (
	"context"
	"net/http"
)

// teamsNotifier posts an Adaptive Card to a Microsoft Teams incoming webhook. We
// target the newer Workflows (Power Automate) format rather than the deprecated
// Office 365 connector MessageCard.
type teamsNotifier struct {
	url   string
	httpc *http.Client
}

// NewTeams builds a Teams notifier for the given Workflows webhook URL.
func NewTeams(url string) Notifier {
	return &teamsNotifier{url: url, httpc: defaultClient()}
}

func (t *teamsNotifier) Notify(ctx context.Context, m Message) error {
	return postJSON(ctx, t.httpc, t.url, teamsCard(m))
}

// teamsCard builds the Workflows Adaptive Card envelope for a Message.
func teamsCard(m Message) map[string]any {
	body := make([]map[string]any, 0, 2)
	if m.Title != "" {
		body = append(body, map[string]any{
			"type": "TextBlock", "text": m.Title, "weight": "Bolder", "size": "Medium", "wrap": true,
		})
	}
	if m.Text != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": m.Text, "wrap": true})
	}

	content := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.2",
		"body":    body,
	}
	if m.Link != "" {
		content["actions"] = []map[string]any{
			{"type": "Action.OpenUrl", "title": "Open", "url": m.Link},
		}
	}

	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{
			{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content":     content,
			},
		},
	}
}
