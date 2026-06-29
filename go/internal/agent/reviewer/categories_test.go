package reviewer

import (
	"testing"

	"automation-agent/internal/githubapi"
)

func TestHasUIFiles(t *testing.T) {
	if hasUIFiles([]githubapi.PRFile{{Path: "internal/x.go"}, {Path: "README.md"}}) {
		t.Error("no UI files should be detected")
	}
	if !hasUIFiles([]githubapi.PRFile{{Path: "internal/x.go"}, {Path: "web/App.tsx"}}) {
		t.Error("App.tsx should be detected as UI")
	}
	if !hasUIFiles([]githubapi.PRFile{{Path: "styles/Main.CSS"}}) {
		t.Error("extension match must be case-insensitive")
	}
}

func TestSelectCategories(t *testing.T) {
	t.Run("no UI files excludes accessibility", func(t *testing.T) {
		cats := selectCategories([]githubapi.PRFile{{Path: "main.go"}})
		for _, c := range cats {
			if c.name == "accessibility" {
				t.Fatal("accessibility must be excluded without UI files")
			}
		}
		if len(cats) != len(categories)-1 {
			t.Errorf("got %d categories, want %d", len(cats), len(categories)-1)
		}
	})

	t.Run("UI files include accessibility", func(t *testing.T) {
		cats := selectCategories([]githubapi.PRFile{{Path: "web/Button.tsx"}})
		var found bool
		for _, c := range cats {
			if c.name == "accessibility" {
				found = true
			}
		}
		if !found {
			t.Error("accessibility must be included with UI files")
		}
		if len(cats) != len(categories) {
			t.Errorf("got %d categories, want all %d", len(cats), len(categories))
		}
	})
}
