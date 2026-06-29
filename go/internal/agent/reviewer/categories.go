package reviewer

import (
	"path"
	"strings"

	"automation-agent/internal/githubapi"
)

// tier selects which model a category runs on: the code-reasoning model (26b) for the lenses
// that need it, the base model (12b) for the lighter ones (spec Decision 3, model-size-split).
type tier int

const (
	tierBase tier = iota // OLLAMA_MODEL (base reasoning)
	tierCode             // OLLAMA_CODE_MODEL (code reasoning)
)

// category is one consolidated review agent. Each bundles related dimensions and emits
// per-dimension-tagged findings over the whole filtered diff (spec Decision 3).
type category struct {
	name       string // unique ADK sub-agent name + state-key suffix
	title      string // human label
	promptName string // prompts/<promptName>.md
	tier       tier
	uiOnly     bool // accessibility runs only when the diff touches UI/markup files
	other      bool // the catch-all: its findings are forced to nitpick
}

// categories is the consolidated agent set (spec Decision 3). The glue/synthesis pass
// (architectural alignment, testability, test coverage) is built separately — it runs after
// these and needs their findings.
var categories = []category{
	{name: "safety", title: "Safety", promptName: "safety", tier: tierCode},
	{name: "security", title: "Security", promptName: "security", tier: tierCode},
	{name: "performance", title: "Performance", promptName: "performance", tier: tierBase},
	{name: "code_quality", title: "Code quality", promptName: "code_quality", tier: tierCode},
	{name: "accessibility", title: "Accessibility", promptName: "accessibility", tier: tierBase, uiOnly: true},
	{name: "other", title: "Other", promptName: "other", tier: tierBase, other: true},
}

// selectCategories returns the categories that apply to a changed-file set: all of them,
// minus the UI-only lens (accessibility) when no UI/markup file changed.
func selectCategories(files []githubapi.PRFile) []category {
	ui := hasUIFiles(files)
	out := make([]category, 0, len(categories))
	for _, c := range categories {
		if c.uiOnly && !ui {
			continue
		}
		out = append(out, c)
	}
	return out
}

// uiExtensions are the file types that warrant an accessibility lens (markup/templates/styles
// and component files).
var uiExtensions = map[string]bool{
	".html": true, ".htm": true, ".xhtml": true, ".css": true, ".scss": true, ".sass": true,
	".less": true, ".jsx": true, ".tsx": true, ".vue": true, ".svelte": true, ".astro": true,
}

// hasUIFiles reports whether any changed file is UI/markup, by extension.
func hasUIFiles(files []githubapi.PRFile) bool {
	for _, f := range files {
		if uiExtensions[strings.ToLower(path.Ext(f.Path))] {
			return true
		}
	}
	return false
}
