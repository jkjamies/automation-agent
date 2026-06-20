package setup

import (
	"testing"

	"google.golang.org/genai"
)

func TestUserTextAndContentText(t *testing.T) {
	c := UserText("hello")
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
	if LastText(nil) != "" {
		t.Error("LastText(nil) should be empty")
	}
	contents := []*genai.Content{UserText("first"), UserText("last")}
	if got := LastText(contents); got != "last" {
		t.Errorf("LastText = %q, want last", got)
	}
}
