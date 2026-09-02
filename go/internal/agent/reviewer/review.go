package reviewer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/model"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

// reviewTrigger / glueTrigger are the user inputs that start each drive. The real instruction
// (lens prompt + diff) lives in the agents' system instruction; these just kick generation.
const (
	reviewTrigger = "Review the diff and report findings as the JSON array specified."
	glueTrigger   = "Synthesize the holistic findings as the JSON array specified."
)

// reviewOutcome is what the model-calling stage hands to publish: the scorecard, the gated
// findings, and one status row per lens that ran.
type reviewOutcome struct {
	card     scorecard
	findings []finding
	lenses   []lensStat
}

// review runs the model-calling stage for a reviewable PR: fan out the category lenses, run the
// holistic glue pass, then apply the deterministic verify gate (confidence drop + dedup) and
// score. It returns the scorecard, the gated findings, and the per-lens status (the caller
// publishes them); it posts nothing itself.
func (e *Engine) review(ctx context.Context, files []githubapi.PRFile, std *standards, stale staleFunc) (reviewOutcome, error) {
	diff := formatDiff(files)
	cats := selectCategories(files)

	category, lenses, err := e.runCategoryReview(ctx, diff, cats, std)
	if err != nil {
		return reviewOutcome{}, fmt.Errorf("reviewer: category review: %w", err)
	}
	// The fan-out is the long pole, so a newer push very plausibly landed during it. Stop before
	// spending the glue call on findings that will be discarded anyway. The check goes here rather
	// than inside each lens: the lenses run concurrently, so a per-lens check would ask GitHub the
	// same question N times at the same instant and still could not stop the calls already in
	// flight — the stage boundary is the only place the answer can change the outcome.
	if stale != nil && stale(ctx) {
		return reviewOutcome{}, errSuperseded
	}
	// Glue sees the category findings as "already reported" and skips re-flagging them, so it must
	// see only the findings that survive the same gates as the final output. Otherwise a finding the
	// verify/citation gate later drops (REVIEW_UNCITED_MODE=drop) is suppressed in glue and then
	// dropped here, vanishing from the review entirely.
	gatedForGlue := e.gateCitations(dropLowConfidence(append([]finding(nil), category...), e.minConfidence), std)
	glue, glueStat, err := e.runGlue(ctx, diff, gatedForGlue, std)
	if err != nil {
		return reviewOutcome{}, fmt.Errorf("reviewer: glue review: %w", err)
	}
	lenses = append(lenses, glueStat)

	all := append(category, glue...)
	all = dropLowConfidence(all, e.minConfidence) // phase-1 verify gate (spec Decision 13)
	all = e.gateCitations(all, std)               // standards citation gate (spec Decision 14)
	all = dedupe(all)                             // cross-lens dedup (spec Decision 3/7)
	return reviewOutcome{card: scoreFindings(all), findings: all, lenses: lenses}, nil
}

// runCategoryReview builds one agent per applicable category, runs them in parallel (ADK
// ParallelAgent — genuine concurrency on Vertex, GPU-serialized locally with no code change,
// spec Decision 17), and returns every category's parsed findings plus one status row per
// category. Empty findings is success (spec Decision 2). The "(other)" catch-all's findings are
// demoted to nitpick.
func (e *Engine) runCategoryReview(ctx context.Context, diff string, cats []category, std *standards) ([]finding, []lensStat, error) {
	agents := make([]agent.Agent, 0, len(cats))
	for _, c := range cats {
		a, err := e.buildCategoryAgent(c, diff, std)
		if err != nil {
			return nil, nil, err
		}
		agents = append(agents, a)
	}
	par, err := parallelagent.New(parallelagent.Config{AgentConfig: agent.Config{
		Name:        "review_all",
		Description: "Per-category review in parallel",
		SubAgents:   agents,
	}})
	if err != nil {
		return nil, nil, fmt.Errorf("build review fan-out: %w", err)
	}
	r, err := setup.NewRunner("reviewer-review", par)
	if err != nil {
		return nil, nil, err
	}
	rep, err := setup.DriveReport(ctx, r, "system", "review", reviewTrigger)
	if err != nil {
		return nil, nil, err
	}

	var out []finding
	lenses := make([]lensStat, 0, len(cats))
	for _, c := range cats {
		v, ok := rep.State[findingsKey(c.name)]
		raw, _ := v.(string)
		if !ok {
			// A lens that ran but found nothing is normal (empty = success); a missing state key
			// means it produced no output at all. Log it for visibility, but don't fail the whole
			// review on one lens — best-effort by design (spec Decision 13).
			e.log.Warn("category produced no findings output", "category", c.name)
		}
		lenses = append(lenses, e.newLensStat(c, rep.Agents[agentName(c)], ok))
		found := parseFindings(raw)
		if c.other {
			found = demoteToNitpick(found)
		}
		out = append(out, found...)
	}
	return out, lenses, nil
}

// runGlue runs the holistic synthesis pass over the diff and the category findings, returning
// the additional architectural/testability/coverage findings it produced and the pass's status
// row. Empty is success.
func (e *Engine) runGlue(ctx context.Context, diff string, prior []finding, std *standards) ([]finding, lensStat, error) {
	a, err := e.buildGlueAgent(diff, prior, std)
	if err != nil {
		return nil, lensStat{}, err
	}
	r, err := setup.NewRunner("reviewer-glue", a)
	if err != nil {
		return nil, lensStat{}, err
	}
	rep, err := setup.DriveReport(ctx, r, "system", "glue", glueTrigger)
	if err != nil {
		return nil, lensStat{}, err
	}
	// Glue writes no state key; its output is the drive's text, so "produced output" is that
	// text being non-blank.
	stat := e.newLensStat(glueLens, rep.Agents[agentName(glueLens)], strings.TrimSpace(rep.Text) != "")
	return parseFindings(rep.Text), stat, nil
}

// newLensStat builds a lens's status row from what the drive observed about its agent. The model
// column prefers the version the adapter reported (a hosted model can resolve an alias to a
// dated version) and falls back to the configured model's name.
func (e *Engine) newLensStat(c category, st setup.AgentStats, ran bool) lensStat {
	modelName := st.Model
	if modelName == "" {
		modelName = e.modelForTier(c.tier).Name()
	}
	return lensStat{lens: c, model: modelName, elapsed: st.Elapsed, tokensIn: st.TokensIn, tokensOut: st.TokensOut, usage: st.Reported, ran: ran}
}

// agentName is the ADK sub-agent name a lens runs under, and so the author its events carry.
func agentName(c category) string { return "review_" + c.name }

// lensStat is one row of the Review details lens table: what a lens ran on, how long it took,
// and what it cost. Its level is not stored here — it is derived from the scorecard at render
// time (lensLevel) so the two tables cannot drift apart.
type lensStat struct {
	lens      category
	model     string
	elapsed   time.Duration
	tokensIn  int
	tokensOut int
	usage     bool // the model reported token counts (false = unknown, not zero)
	ran       bool // the lens produced output at all
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

// buildReviewInstruction composes a category agent's instruction: the lens prompt, the repo's
// standards rule menu (when any), and the filtered diff (baked in because they are per-event).
func buildReviewInstruction(promptBody, diff string, std *standards) string {
	var b strings.Builder
	b.WriteString(promptBody)
	writeStandardsMenu(&b, std)
	b.WriteString("\n\n## Diff under review\n\n")
	b.WriteString(diff)
	return b.String()
}

// buildGlueInstruction composes the glue agent's instruction: the glue prompt, the standards menu,
// the diff, and the findings the category agents already produced (so it reasons holistically
// without re-flagging them).
func buildGlueInstruction(promptBody, diff string, prior []finding, std *standards) string {
	var b strings.Builder
	b.WriteString(promptBody)
	writeStandardsMenu(&b, std)
	b.WriteString("\n\n## Diff under review\n\n")
	b.WriteString(diff)
	b.WriteString("\n\n## Findings already reported by other lenses\n\n")
	b.WriteString(findingsJSON(prior))
	return b.String()
}

// writeStandardsMenu appends the repo's compact rule menu and the citation instruction to an agent
// prompt when standards were discovered. The full text of any rule is available via get_rule.
func writeStandardsMenu(b *strings.Builder, std *standards) {
	if std.empty() {
		return
	}
	b.WriteString("\n\n## Repo standards (cite rule_id for conformance findings)\n\n")
	b.WriteString(std.menu())
	b.WriteString("\nWhen a finding is a violation of one of these rules, set its dimension to the " +
		"rule's dimension and set \"rule_id\" to the rule's id. Call get_rule(id) to read a rule's " +
		"full text before flagging. Never invent a rule id; a pattern/architecture finding with no " +
		"matching rule is not a standards violation.\n")
}
