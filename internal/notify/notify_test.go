package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureServer records the body of the next POST it receives.
func captureServer(t *testing.T, status int) (*httptest.Server, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = b
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestSlackNotify(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	n := NewSlack(srv.URL)

	if err := n.Notify(context.Background(), Message{Title: "Digest", Text: "3 commits", Link: "https://x/pr/1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "*Digest*\n3 commits\n<https://x/pr/1>"
	if payload["text"] != want {
		t.Errorf("text = %q, want %q", payload["text"], want)
	}
}

func TestTeamsNotify(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	n := NewTeams(srv.URL)

	if err := n.Notify(context.Background(), Message{Title: "Result", Text: "fixed", Link: "https://x/pr/2"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "message" {
		t.Errorf("type = %v, want message", payload["type"])
	}
	atts, ok := payload["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %v", payload["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("contentType = %v", att["contentType"])
	}
	content := att["content"].(map[string]any)
	if content["type"] != "AdaptiveCard" {
		t.Errorf("content type = %v", content["type"])
	}
	if _, hasActions := content["actions"]; !hasActions {
		t.Error("expected actions for a message with a link")
	}
}

func TestNon2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	if err := NewSlack(srv.URL).Notify(context.Background(), Message{Text: "x"}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestNewFactory(t *testing.T) {
	if _, err := New("slack", "https://hook", ""); err != nil {
		t.Errorf("slack: %v", err)
	}
	if _, err := New("teams", "", "https://hook"); err != nil {
		t.Errorf("teams: %v", err)
	}
	if _, err := New("slack", "", ""); err == nil {
		t.Error("slack without URL should error")
	}
	if _, err := New("discord", "a", "b"); err == nil {
		t.Error("unknown provider should error")
	}
}

func TestTeamsCardOmitsActionsWithoutLink(t *testing.T) {
	card := teamsCard(Message{Title: "t", Text: "b"})
	content := card["attachments"].([]map[string]any)[0]["content"].(map[string]any)
	if _, has := content["actions"]; has {
		t.Error("no link -> no actions")
	}
}
