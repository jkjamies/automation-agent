package reviewer

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"automation-agent/internal/githubapi"
)

// The scorecard groups by dimension and the lens table groups by lens; they stay in sync only if
// every dimension a finding can carry is owned by exactly one lens.
func TestLensesOwnEveryDimensionOnce(t *testing.T) {
	owners := map[Dimension][]string{}
	for _, l := range allLenses() {
		if len(l.dims) == 0 {
			t.Errorf("lens %s owns no dimensions", l.name)
		}
		for _, d := range l.dims {
			if !knownDimensions[d] {
				t.Errorf("lens %s owns unknown dimension %q", l.name, d)
			}
			owners[d] = append(owners[d], l.name)
		}
	}
	for d := range knownDimensions {
		if n := len(owners[d]); n != 1 {
			t.Errorf("dimension %q owned by %v, want exactly one lens", d, owners[d])
		}
	}
}

// dimensionLine matches the schema line each lens prompt shows the model, e.g.
// `"dimension": "runtime_safety" | "error_handling",`.
var dimensionLine = regexp.MustCompile(`(?m)^\s*"dimension":\s*(.+?),?$`)

// A lens's owned dimensions are exactly the values its prompt lets the model emit. Otherwise a
// prompt edit could add a dimension the table credits to no lens, or a lens could claim one its
// prompt never produces.
func TestLensPromptsMatchOwnedDimensions(t *testing.T) {
	for _, l := range allLenses() {
		body, err := prompts.Get(l.promptName)
		if err != nil {
			t.Fatalf("%s: %v", l.name, err)
		}
		m := dimensionLine.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s prompt has no \"dimension\" schema line", l.name)
		}
		fromPrompt := map[Dimension]bool{}
		for _, q := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(m[1], -1) {
			fromPrompt[Dimension(q[1])] = true
		}
		owned := map[Dimension]bool{}
		for _, d := range l.dims {
			owned[d] = true
		}
		for d := range fromPrompt {
			if !owned[d] {
				t.Errorf("%s prompt emits %q which the lens does not own", l.name, d)
			}
		}
		for d := range owned {
			if !fromPrompt[d] {
				t.Errorf("%s owns %q which its prompt never emits", l.name, d)
			}
		}
	}
}

// A lens's level is the worst scorecard level among the dimensions it owns — computed from the
// same per-dimension levels the scorecard table shows, so the two tables agree by construction.
// A finding is credited to the lens owning its dimension regardless of which agent emitted it.
func TestLensLevelFollowsScorecard(t *testing.T) {
	card := scoreFindings([]finding{
		f(DimSecurity, SeverityCritical),                                          // security: red
		f(DimReadability, SeverityMajor),                                          // readability: yellow
		f(DimDocumentation, SeverityMedium), f(DimErrorHandling, SeverityNitpick), // green dims
	})
	byName := map[string]category{}
	for _, l := range allLenses() {
		byName[l.name] = l
	}
	want := map[string]level{"security": levelRed, "code_quality": levelYellow, "safety": levelGreen, "performance": levelGreen, "glue": levelGreen}
	for name, lvl := range want {
		if got := lensLevel(card, byName[name]); got != lvl {
			t.Errorf("lensLevel(%s) = %v, want %v", name, got, lvl)
		}
	}
	// Every red/yellow dimension on the scorecard must be visible through some lens's level.
	for _, d := range card.dims {
		covered := false
		for _, l := range allLenses() {
			if lensLevel(card, l) >= d.level {
				for _, own := range l.dims {
					if own == d.dimension {
						covered = true
					}
				}
			}
		}
		if !covered {
			t.Errorf("dimension %s (%v) is not reflected in any lens level", d.dimension, d.level)
		}
	}
}

func TestLensTable(t *testing.T) {
	card := scoreFindings([]finding{f(DimSecurity, SeverityCritical)})
	lenses := []lensStat{
		{lens: categories[1], model: "gemma3:27b", elapsed: 12340 * time.Millisecond, tokensIn: 8120, tokensOut: 240, usage: true, ran: true},
		{lens: categories[2], model: "gemma3:12b", elapsed: 900 * time.Millisecond, ran: true},
		{lens: categories[4], skipped: true},
		{lens: glueLens, model: "gemma3:27b", ran: false},
	}
	got := lensTable(card, lenses)
	for _, want := range []string{
		"| Lens | Level | Model | Time | Tokens in | Tokens out |",
		"| Security | 🔴 | `gemma3:27b` | 12.3s | 8120 | 240 |",
		"| Performance | 🟢 | `gemma3:12b` | 0.9s | – | – |", // ran, no usage reported: dashes, not zeros
		"| Accessibility | – | – | – | – | – |",             // not selected for this diff: did not apply
		"| Holistic synthesis | ⚪ no output | `gemma3:27b` | – | – | – |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lens table missing %q:\n%s", want, got)
		}
	}
}

// classify partitions the gated findings: every finding lands in exactly one of inline,
// out-of-diff, or nitpicks, and their sum is the scorecard's total — so the header count, the
// collapsible sections, and the scorecard describe the same set.
func TestClassifyPartitionsEveryFinding(t *testing.T) {
	files := []githubapi.PRFile{{Path: "a.go", Status: "modified", Patch: "@@ -1,2 +1,3 @@\n a\n+b\n+c\n"}}
	findings := []finding{
		{File: "a.go", Line: 2, Dimension: DimSecurity, Severity: SeverityCritical, Message: "in diff"},
		{File: "a.go", Line: 99, Dimension: DimPerformance, Severity: SeverityMajor, Message: "out of diff"},
		{File: "a.go", Line: 3, Dimension: DimReadability, Severity: SeverityNitpick, Message: "nit in diff"},
		{File: "", Line: 0, Dimension: DimTestCoverage, Severity: SeverityMedium, Message: "no location"},
	}
	inline, outOfDiff, nitpicks := classify(findings, newDiffIndex(files))
	if got := len(inline) + len(outOfDiff) + len(nitpicks); got != len(findings) {
		t.Fatalf("classified %d of %d findings", got, len(findings))
	}
	if card := scoreFindings(findings); card.total != len(inline)+len(outOfDiff)+len(nitpicks) {
		t.Errorf("scorecard total %d != classified %d", card.total, len(inline)+len(outOfDiff)+len(nitpicks))
	}
	if len(inline) != 1 || len(outOfDiff) != 2 || len(nitpicks) != 1 {
		t.Errorf("partition = inline %d / out %d / nits %d, want 1/2/1", len(inline), len(outOfDiff), len(nitpicks))
	}
}

// The two tables deliberately differ in what they list: the scorecard shows only dimensions that
// received findings, while the lens table shows every lens (skipped or silent ones included).
func TestScorecardTableListsOnlyDimensionsWithFindings(t *testing.T) {
	got := scorecardTable(scoreFindings([]finding{f(DimSecurity, SeverityMajor)}))
	if !strings.Contains(got, "| security |") {
		t.Errorf("scorecard missing the scored dimension:\n%s", got)
	}
	for _, d := range []Dimension{DimPerformance, DimAccessibility, DimTestCoverage, DimOther} {
		if strings.Contains(got, "| "+string(d)+" |") {
			t.Errorf("scorecard lists %s, which has no findings:\n%s", d, got)
		}
	}
	if !strings.Contains(scorecardTable(scoreFindings(nil)), "No findings") {
		t.Error("an empty scorecard must say so rather than render an empty table")
	}
}
