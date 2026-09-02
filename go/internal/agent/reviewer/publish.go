package reviewer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"automation-agent/internal/githubapi"
)

// checkName is the advisory check the reviewer publishes (agent-published, human-consumed —
// distinct from the fixers' agent-*-verify checks that the agent reads). Globally unique and
// identical across ports (external contract).
const checkName = "agent-review"

// publishMeta carries the per-PR identifiers and context the published artifacts need.
type publishMeta struct {
	owner, repo string
	number      int
	headSHA     string
	files       []githubapi.PRFile // for the in-diff index
	lenses      []lensStat         // per-lens status rows for the Review details section
	standards   []string           // applied standards source paths (nil = generic), for reporting
}

// summaryMarker is the hidden HTML comment that identifies the reviewer's single summary comment
// so a re-review updates it rather than posting a new one (spec Decision 9).
func summaryMarker(owner, repo string, number int) string {
	return fmt.Sprintf("<!-- automation-agent:review:%s/%s#%d -->", owner, repo, number)
}

// publish posts the review for a scored PR: inline comments for in-diff actionable findings, a
// marker-updated summary comment with the scorecard, and the advisory agent-review check. Out-of-
// diff actionable findings and nitpicks go into the summary (never dropped, spec Decision 6).
func (e *Engine) publish(ctx context.Context, card scorecard, findings []finding, meta publishMeta) error {
	// At-least-once safety: reconciliation makes the inline comments idempotent, but the check run
	// and summary are create/upsert-only, so a redelivered task for a SHA already published would
	// duplicate the check. If the agent-review check already exists for this head SHA, skip — a
	// genuine re-push carries a new SHA and still reconciles below.
	if e.alreadyPublished(ctx, meta) {
		e.log.Info("review already published for head SHA; skipping re-post", "repo", meta.owner+"/"+meta.repo, "sha", meta.headSHA)
		return nil
	}
	idx := newDiffIndex(meta.files)
	inline, outOfDiff, nitpicks := classify(findings, idx)
	actionable := len(inline) + len(outOfDiff)

	// Reconcile against the comments already on the PR (GitHub-as-store): keep inline findings that
	// still apply (don't re-post — so a re-review is idempotent), post only new ones, and minimize
	// the comments whose finding is gone (fixed or no longer relevant). This makes re-publishing on
	// a redelivered task or a re-push safe (spec Decisions 6/11).
	existing, err := e.gh.ListReviewComments(ctx, meta.owner, meta.repo, meta.number)
	if err != nil {
		return fmt.Errorf("reviewer: list existing comments: %w", err)
	}
	rec := reconcile(inline, existing)

	// Post only the new inline findings; an empty review is noise.
	if len(rec.toPost) > 0 {
		comments := make([]githubapi.ReviewComment, 0, len(rec.toPost))
		for _, f := range rec.toPost {
			comments = append(comments, githubapi.ReviewComment{Path: f.File, Line: f.Line, Side: "RIGHT", Body: inlineCommentBody(f)})
		}
		body := fmt.Sprintf("%s Agent review — see the summary comment for the full scorecard.", card.overall)
		// Pin the review to the SHA the findings were computed against. meta.files (and so every
		// line number in comments) came from this SHA's diff; without the pin GitHub would resolve
		// the lines against whatever HEAD is current when this call lands.
		if err := e.gh.CreateReview(ctx, meta.owner, meta.repo, meta.number, githubapi.ReviewInput{Body: body, Comments: comments, CommitID: meta.headSHA}); err != nil {
			return fmt.Errorf("reviewer: post review: %w", err)
		}
	}

	// Minimize the comments whose finding no longer applies — best-effort. New inline comments are
	// already posted but the summary and check are not; aborting here on a single minimize failure
	// would leave the PR without its summary/check, and a retry would short-circuit at
	// alreadyPublished once the check exists. So log and continue per node, letting the summary and
	// check still publish. A leftover stale comment is collapsed on the next genuine re-push.
	for _, id := range rec.toMinimize {
		if err := e.gh.MinimizeComment(ctx, id); err != nil {
			e.log.Warn("reviewer: minimize outdated comment failed; continuing", "repo", meta.owner+"/"+meta.repo, "node", id, "err", err)
		}
	}

	marker := summaryMarker(meta.owner, meta.repo, meta.number)
	if err := e.gh.UpsertMarkerComment(ctx, meta.owner, meta.repo, meta.number, marker, summaryComment(marker, card, actionable, nitpicks, outOfDiff, meta)); err != nil {
		return fmt.Errorf("reviewer: upsert summary comment: %w", err)
	}

	if err := e.gh.CreateCheckRun(ctx, meta.owner, meta.repo, githubapi.CheckRunInput{
		Name:       checkName,
		HeadSHA:    meta.headSHA,
		Conclusion: checkConclusion(card.overall),
		Title:      fmt.Sprintf("%s Agent review — %s", card.overall, levelWord(card.overall)),
		Summary:    fmt.Sprintf("Overall: %s · Actionable comments: %d", levelWord(card.overall), actionable),
	}); err != nil {
		return fmt.Errorf("reviewer: create check: %w", err)
	}
	return nil
}

// publishDeny posts the "too large to review" outcome (spec Decision 4): a marker-updated summary
// comment framed fail-like (🔴) plus a neutral check. No model call was made.
func (e *Engine) publishDeny(ctx context.Context, meta publishMeta, reason string, files, diffBytes int) error {
	if e.alreadyPublished(ctx, meta) {
		e.log.Info("deny already published for head SHA; skipping re-post", "repo", meta.owner+"/"+meta.repo, "sha", meta.headSHA)
		return nil
	}
	marker := summaryMarker(meta.owner, meta.repo, meta.number)
	body := fmt.Sprintf("%s\n## 🔴 Agent review — too large for automated review\n\nThis PR is too large to review automatically (%d files / %d bytes after excluding generated files). Please split it into smaller PRs.\n\n_%s_\n",
		marker, files, diffBytes, reason)
	if err := e.gh.UpsertMarkerComment(ctx, meta.owner, meta.repo, meta.number, marker, body); err != nil {
		return fmt.Errorf("reviewer: upsert deny comment: %w", err)
	}
	if err := e.gh.CreateCheckRun(ctx, meta.owner, meta.repo, githubapi.CheckRunInput{
		Name:       checkName,
		HeadSHA:    meta.headSHA,
		Conclusion: "neutral",
		Title:      "🔴 Agent review — too large",
		Summary:    fmt.Sprintf("%d files / %d bytes after excluding generated files; please split.", files, diffBytes),
	}); err != nil {
		return fmt.Errorf("reviewer: create deny check: %w", err)
	}
	return nil
}

// alreadyPublished reports whether the agent-review check already exists for the head SHA — i.e.
// this SHA was already published and a redelivered task should not re-post. A lookup error is
// treated as "not published" so a transient failure never suppresses a real review.
func (e *Engine) alreadyPublished(ctx context.Context, meta publishMeta) bool {
	res, err := e.gh.AgentCheck(ctx, meta.owner, meta.repo, meta.headSHA, checkName)
	return err == nil && res.Found
}

// classify splits confidence-gated findings into inline findings (actionable, on a commentable diff
// line — these become review comments after reconciliation), out-of-diff actionable findings (listed
// in the summary, never snapped to a wrong line — spec Decision 6), and nitpicks (collapsed in the
// summary).
func classify(findings []finding, idx diffIndex) (inline, outOfDiff, nitpicks []finding) {
	for _, f := range findings {
		if f.Severity == SeverityNitpick {
			nitpicks = append(nitpicks, f)
			continue
		}
		if f.File != "" && f.Line > 0 && idx.inDiff(f.File, f.Line) {
			inline = append(inline, f)
			continue
		}
		outOfDiff = append(outOfDiff, f)
	}
	return inline, outOfDiff, nitpicks
}

// inlineCommentBody renders one inline comment: an icon+category prefix, the message, an optional
// plain-language suggestion, and an optional "Prompt for AI agents" block (spec Decisions 9/10).
//
// The suggestion is prose, deliberately not a GitHub ```suggestion block. That block is a
// one-click commit of a verbatim replacement for the commented lines, and the lenses cannot
// author one reliably: they see a unified diff with no surrounding file, so a snippet they
// write rarely aligns with the exact lines it would replace, and "apply" then commits broken
// code. Worse, the model-authored "snippet" is often itself a sentence, which the block would
// offer to commit as source. A sentence describing the change is what the reader can act on
// with the context they have; the fenced fix prompt below is the hand-off for an agent that
// does have the file.
func inlineCommentBody(f finding) string {
	var b strings.Builder
	// Dimension/severity are normalized to known enums, so only the model-authored message needs
	// sanitizing here.
	fmt.Fprintf(&b, "**%s** · _%s_\n\n%s\n", findingPrefix(f), f.Dimension, sanitizeText(f.Message))
	if s := strings.TrimSpace(f.Suggestion); s != "" {
		// Model-authored prose, so it gets the same sanitizing as the message.
		fmt.Fprintf(&b, "\n**Suggestion:** %s\n", sanitizeText(s))
	}
	if f.FixPrompt != "" {
		// FixPrompt is model-authored; render it inside a code fence so any @mentions or HTML are
		// literal (not pinged/injected) and it stays copy-pasteable. Size the fence past any
		// backtick run in the prompt so the content cannot break out.
		fence := strings.Repeat("`", maxBacktickRun(f.FixPrompt)+1)
		if len(fence) < 3 {
			fence = "```"
		}
		b.WriteString("\n<details>\n<summary>🤖 Prompt for AI agents</summary>\n\n")
		b.WriteString(fence + "\n")
		b.WriteString(f.FixPrompt)
		if !strings.HasSuffix(f.FixPrompt, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(fence + "\n\n</details>\n")
	}
	// Hidden fingerprint marker so a later re-review re-identifies this comment and reconciles it
	// (GitHub-as-store); it renders invisibly.
	b.WriteString("\n" + fpMarker(f.fingerprint()) + "\n")
	return b.String()
}

// sanitizeText neutralizes model-authored text for safe embedding in a Markdown comment: it
// escapes HTML-significant characters (so a finding can't inject markup such as </details>) and
// breaks @mentions with a zero-width space (so the reviewer never pings a real user). The fenced
// FixPrompt is left untouched by callers — a code fence already renders its content literally.
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return mentionPattern.ReplaceAllString(s, "@\u200b$1")
}

// mentionPattern matches an @ immediately followed by a mention character; sanitizeText inserts a
// zero-width space after the @ so GitHub does not render (and notify) it as a mention.
var mentionPattern = regexp.MustCompile(`@([A-Za-z0-9])`)

// findingPrefix is the icon+category label that leads an inline comment (spec Decision 9).
func findingPrefix(f finding) string {
	if f.Dimension == DimSecurity {
		return "🔒 Security"
	}
	switch f.Severity {
	case SeverityCritical, SeverityMajor:
		return "⚠️ Potential issue"
	default:
		return "🛠️ Refactor"
	}
}

// summaryComment assembles the marker-updated summary comment (spec Decision 9): header, scorecard
// table, and collapsible sections for nitpicks, out-of-diff findings, and review details.
func summaryComment(marker string, card scorecard, actionable int, nitpicks, outOfDiff []finding, meta publishMeta) string {
	var b strings.Builder
	b.WriteString(marker)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "## %s Agent review — Overall: %s · Actionable comments: %d\n\n", card.overall, levelWord(card.overall), actionable)
	b.WriteString(scorecardTable(card))
	if len(nitpicks) > 0 {
		b.WriteString(collapsible(fmt.Sprintf("🧹 Nitpicks (%d)", len(nitpicks)), findingsList(nitpicks)))
	}
	if len(outOfDiff) > 0 {
		b.WriteString(collapsible(fmt.Sprintf("🔭 Outside diff range (%d)", len(outOfDiff)), findingsList(outOfDiff)))
	}
	b.WriteString(collapsible("Review details", reviewDetails(card, meta)))
	return b.String()
}

// scorecardTable renders the per-dimension severity histogram (spec Decision 5). With no findings
// it states so rather than emitting an empty table.
func scorecardTable(card scorecard) string {
	if len(card.dims) == 0 {
		return "_No findings._\n\n"
	}
	var b strings.Builder
	b.WriteString("| Dimension | Level | Critical | Major | Medium | Nitpick |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, d := range card.dims {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d |\n", d.dimension, d.level, d.critical, d.major, d.medium, d.nitpick)
	}
	b.WriteByte('\n')
	return b.String()
}

// findingsList renders findings as a bulleted file:line list for the summary's collapsible
// sections (out-of-diff findings and nitpicks).
func findingsList(fs []finding) string {
	var b strings.Builder
	for _, f := range fs {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "- **%s** `%s` _(%s)_ — %s\n", f.Severity, loc, f.Dimension, sanitizeText(f.Message))
	}
	return b.String()
}

// reviewDetails renders the "Review details" section: head SHA, file count, the standards
// applied, and the per-lens status table.
func reviewDetails(card scorecard, meta publishMeta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- Head SHA: `%s`\n", meta.headSHA)
	fmt.Fprintf(&b, "- Files reviewed: %d\n", len(meta.files))
	if len(meta.standards) > 0 {
		fmt.Fprintf(&b, "- Standards applied: %s\n", strings.Join(meta.standards, ", "))
	} else {
		// Empty also covers standards-off and the discovery/distill fallback, not just a repo with
		// no convention docs — so stay neutral rather than asserting none were found.
		b.WriteString("- Standards: generic review\n")
	}
	if len(meta.lenses) > 0 {
		b.WriteByte('\n')
		b.WriteString(lensTable(card, meta.lenses))
	}
	return b.String()
}

// lensTable renders one status row per lens — every category and the glue pass, whether or not
// it ran, unlike the scorecard, which lists only dimensions with findings: its level (derived
// from the scorecard, see lensLevel), the model it ran on, how long it took, and the tokens it
// consumed. A lens the diff did not select is marked skipped, and one that produced no output is
// marked so a silent lens is visible rather than reading as a clean one; token cells show "–"
// when the model reported no usage, which is not the same as zero.
func lensTable(card scorecard, lenses []lensStat) string {
	var b strings.Builder
	b.WriteString("| Lens | Level | Model | Time | Tokens in | Tokens out |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, l := range lenses {
		if l.skipped {
			fmt.Fprintf(&b, "| %s | ⏭️ skipped (no UI files changed) | – | – | – | – |\n", l.lens.title)
			continue
		}
		lvl := lensLevel(card, l.lens).String()
		if !l.ran {
			lvl = "⚪ no output"
		}
		in, out := "–", "–"
		if l.usage {
			in, out = fmt.Sprint(l.tokensIn), fmt.Sprint(l.tokensOut)
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %s | %s |\n", l.lens.title, lvl, l.model, formatElapsed(l.elapsed), in, out)
	}
	return b.String()
}

// formatElapsed renders a lens duration for the table as seconds to one decimal — "0.9s",
// "12.3s", "74.5s" — one unit down the column so rows compare at a glance; "–" when nothing
// was observed.
func formatElapsed(d time.Duration) string {
	if d <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// collapsible wraps body in a <details> block with the given summary label.
func collapsible(summary, body string) string {
	return fmt.Sprintf("\n<details>\n<summary>%s</summary>\n\n%s\n</details>\n", summary, body)
}

// levelWord is the textual grade shown beside the glyph in headers and the check.
func levelWord(l level) string {
	switch l {
	case levelRed:
		return "Red"
	case levelYellow:
		return "Yellow"
	default:
		return "Green"
	}
}

// checkConclusion maps the overall grade to the advisory check conclusion (spec Decision 15):
// green is success; yellow and red are neutral. It is never failure — the reviewer never gates a
// merge.
func checkConclusion(overall level) string {
	if overall == levelGreen {
		return "success"
	}
	return "neutral"
}
