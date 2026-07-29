package setup

import (
	"testing"

	"google.golang.org/genai"
)

func TestUserTextAndContentText(t *testing.T) {
	c := userText("hello")
	if c.Role != genai.RoleUser {
		t.Errorf("role = %q, want user", c.Role)
	}
	if ContentText(c) != "hello" {
		t.Errorf("ContentText = %q", ContentText(c))
	}
	if ContentText(nil) != "" {
		t.Error("ContentText(nil) should be empty")
	}
}

func TestLastText(t *testing.T) {
	if lastText(nil) != "" {
		t.Error("lastText(nil) should be empty")
	}
	contents := []*genai.Content{userText("first"), userText("last")}
	if got := lastText(contents); got != "last" {
		t.Errorf("lastText = %q, want last", got)
	}
}
