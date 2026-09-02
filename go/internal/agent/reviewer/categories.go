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
	// dims are the dimensions this lens owns — exactly the values its prompt allows a finding to
	// carry. The scorecard groups by dimension and the lens status table groups by lens; a lens's
	// level is derived from its dims' scorecard levels, so the two can never disagree. Every
	// known dimension belongs to exactly one lens (asserted by tests against the prompts too).
	dims   []Dimension
	uiOnly bool // accessibility runs only when the diff touches UI/markup files
	other  bool // the catch-all: its findings are forced to nitpick
}

// categories is the consolidated agent set (spec Decision 3). The glue/synthesis pass
// (architectural alignment, testability, test coverage) is built separately — it runs after
// these and needs their findings.
var categories = []category{
	{name: "safety", title: "Safety", promptName: "safety", tier: tierCode,
		dims: []Dimension{DimRuntimeSafety, DimErrorHandling}},
	{name: "security", title: "Security", promptName: "security", tier: tierCode,
		dims: []Dimension{DimSecurity}},
	{name: "performance", title: "Performance", promptName: "performance", tier: tierBase,
		dims: []Dimension{DimPerformance}},
	{name: "code_quality", title: "Code quality", promptName: "code_quality", tier: tierCode,
		dims: []Dimension{DimPatternViolation, DimMaintainability, DimReadability, DimDocumentation}},
	{name: "accessibility", title: "Accessibility", promptName: "accessibility", tier: tierBase, uiOnly: true,
		dims: []Dimension{DimAccessibility}},
	{name: "other", title: "Other", promptName: "other", tier: tierBase, other: true,
		dims: []Dimension{DimOther}},
}

// glueLens is the glue/synthesis pass described as a lens, so it is named, prompted, and
// reported the same way as the categories. It is not in categories because it runs after them
// and needs their findings.
var glueLens = category{name: "glue", title: "Holistic synthesis", promptName: "glue", tier: tierCode,
	dims: []Dimension{DimArchitecture, DimTestability, DimTestCoverage}}

// allLenses is every lens that can contribute to a review, in report order: the categories,
// then glue.
func allLenses() []category { return append(append([]category(nil), categories...), glueLens) }

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
