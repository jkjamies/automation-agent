package reviewer

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/model"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

// reviewTrigger / glueTrigger are the user inputs that start each drive. The real instruction
// (lens prompt + diff) lives in the agents' system instruction; these just kick generation.
const (
	reviewTrigger = "Review the diff and report findings as the JSON array specified."
	glueTrigger   = "Synthesize the holistic findings as the JSON array specified."
)

// review runs the model-calling stage for a reviewable PR: fan out the category lenses, run
// the holistic glue pass, then apply the deterministic verify gate (confidence drop + dedup)
// and score. It posts nothing — publishing the scorecard and inline comments lands later.
func (e *Engine) review(ctx context.Context, files []githubapi.PRFile) (scorecard, error) {
	diff := formatDiff(files)
	cats := selectCategories(files)

	category, err := e.runCategoryReview(ctx, diff, cats)
	if err != nil {
		return scorecard{}, fmt.Errorf("reviewer: category review: %w", err)
	}
	glue, err := e.runGlue(ctx, diff, category)
	if err != nil {
		return scorecard{}, fmt.Errorf("reviewer: glue review: %w", err)
	}

	all := append(category, glue...)
	all = dropLowConfidence(all, e.minConfidence) // phase-1 verify gate (spec Decision 13)
	all = dedupe(all)                             // cross-lens dedup (spec Decision 3/7)
	return scoreFindings(all), nil
}

// runCategoryReview builds one agent per applicable category, runs them in parallel (ADK
// ParallelAgent — genuine concurrency on Vertex, GPU-serialized locally with no code change,
// spec Decision 17), and returns every category's parsed findings. Empty findings is success
// (spec Decision 2). The "(other)" catch-all's findings are demoted to nitpick.
func (e *Engine) runCategoryReview(ctx context.Context, diff string, cats []category) ([]Finding, error) {
	agents := make([]agent.Agent, 0, len(cats))
	for _, c := range cats {
		a, err := e.buildCategoryAgent(c, diff)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	par, err := parallelagent.New(parallelagent.Config{AgentConfig: agent.Config{
		Name:        "review_all",
		Description: "Per-category review in parallel",
		SubAgents:   agents,
	}})
	if err != nil {
		return nil, fmt.Errorf("build review fan-out: %w", err)
	}
	r, err := setup.NewRunner("reviewer-review", par)
	if err != nil {
		return nil, err
	}
	state, err := setup.DriveCollectState(ctx, r, "system", "review", reviewTrigger)
	if err != nil {
		return nil, err
	}

	var out []Finding
	for _, c := range cats {
		v, ok := state[findingsKey(c.name)]
		raw, _ := v.(string)
		if !ok {
			// A lens that ran but found nothing is normal (empty = success); a missing state key
			// means it produced no output at all. Log it for visibility, but don't fail the whole
			// review on one lens — best-effort by design (spec Decision 13).
			e.log.Warn("category produced no findings output", "category", c.name)
		}
		found := parseFindings(raw)
		if c.other {
			found = demoteToNitpick(found)
		}
		out = append(out, found...)
	}
	return out, nil
}

// runGlue runs the holistic synthesis pass over the diff and the category findings, returning
// the additional architectural/testability/coverage findings it produced. Empty is success.
func (e *Engine) runGlue(ctx context.Context, diff string, prior []Finding) ([]Finding, error) {
	a, err := e.buildGlueAgent(diff, prior)
	if err != nil {
		return nil, err
	}
	r, err := setup.NewRunner("reviewer-glue", a)
	if err != nil {
		return nil, err
	}
	text, err := setup.DriveText(ctx, r, "system", "glue", glueTrigger)
	if err != nil {
		return nil, err
	}
	return parseFindings(text), nil
}

// formatDiff renders the filtered files as one prompt-ready diff: a header per file plus its
// patch in a fenced block. A file with no patch (binary/oversized) is noted so the model knows
// it changed without a hunk to review.
func formatDiff(files []githubapi.PRFile) string {
	var b strings.Builder
	for _, f := range files {
		if f.Status == "renamed" && f.PreviousPath != "" {
			fmt.Fprintf(&b, "### %s (renamed from %s)\n", f.Path, f.PreviousPath)
		} else {
			fmt.Fprintf(&b, "### %s (%s)\n", f.Path, f.Status)
		}
		if strings.TrimSpace(f.Patch) == "" {
			b.WriteString("(no textual diff available)\n\n")
			continue
		}
		// Patch content is untrusted (it can be a diff of a Markdown/RST file that itself contains
		// ``` runs), so pick a fence longer than the longest backtick run in the patch — otherwise
		// an embedded run would close the block early and corrupt the prompt structure.
		fence := strings.Repeat("`", maxBacktickRun(f.Patch)+1)
		if len(fence) < 3 {
			fence = "```"
		}
		b.WriteString(fence)
		b.WriteString("diff\n")
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(fence)
		b.WriteString("\n\n")
	}
	return b.String()
}

// maxBacktickRun returns the length of the longest run of consecutive backticks in s (0 if
// none), used to size a fence that the content cannot break out of.
func maxBacktickRun(s string) int {
	longest, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	return longest
}

// findingsKey is the session-state key a category agent writes its findings JSON to.
func findingsKey(name string) string { return "findings:" + name }

// modelForTier returns the LLM a category runs on (code tier → code model, else base model).
func (e *Engine) modelForTier(t tier) model.LLM {
	if t == tierCode {
		return e.codeLLM
	}
	return e.baseLLM
}

// buildReviewInstruction composes a category agent's instruction: the lens prompt followed by the
// filtered diff (baked in because it is per-event).
func buildReviewInstruction(promptBody, diff string) string {
	var b strings.Builder
	b.WriteString(promptBody)
	b.WriteString("\n\n## Diff under review\n\n")
	b.WriteString(diff)
	return b.String()
}

// buildGlueInstruction composes the glue agent's instruction: the glue prompt, the diff, and the
// findings the category agents already produced (so it reasons holistically without re-flagging
// them).
func buildGlueInstruction(promptBody, diff string, prior []Finding) string {
	var b strings.Builder
	b.WriteString(promptBody)
	b.WriteString("\n\n## Diff under review\n\n")
	b.WriteString(diff)
	b.WriteString("\n\n## Findings already reported by other lenses\n\n")
	b.WriteString(findingsJSON(prior))
	return b.String()
}
