package fixflow

import (
	"fmt"
	"strings"

	"automation-agent/internal/githubapi"
)

// terminalOutcome is the way a fix run ended; it selects the summary framing.
type terminalOutcome int

const (
	outcomeSuccess terminalOutcome = iota
	outcomeExhausted
	outcomeTimeout
	outcomeClean // triage found nothing to address — already clean, no PR opened
)

// summaryInput is everything a terminal summary needs. The per-attempt work product lives
// only in the PR (commits + diff), never the session, so `changed` (a base...branch
// comparison) is how the human learns what the agent actually did.
type summaryInput struct {
	outcome    terminalOutcome
	workflow   string // spec.Name (lint | coverage)
	fullRepo   string
	prNumber   int
	attempts   int
	report     string // original targeted findings (runParams.report)
	lastOutput string // last CI check output (exhausted) — the remaining findings
	timeout    string // CI_TIMEOUT (timeout outcome)
	checkName  string // the awaited check (timeout outcome)
	changed    githubapi.Comparison
}

// maxFindingsRunes bounds how much of a (potentially large) findings blob a summary inlines.
const maxFindingsRunes = 280

// buildSummaryText frames a terminal outcome into a human notification body, enriched with
// the original findings and what changed on the PR. Pure (no I/O) so it is unit-testable.
func buildSummaryText(in summaryInput) string {
	changed := changedSummary(in.changed)
	switch in.outcome {
	case outcomeSuccess:
		text := fmt.Sprintf("%s: the %s fix passed CI after %s. %s", in.fullRepo, in.workflow, attemptsPhrase(in.attempts), changed)
		return appendFindings(text, "Targeted", in.report)
	case outcomeExhausted:
		text := fmt.Sprintf("%s: the %s fix still fails CI after %s. Please review. %s", in.fullRepo, in.workflow, attemptsPhrase(in.attempts), changed)
		return appendFindings(text, "Remaining", in.lastOutput)
	case outcomeTimeout:
		text := fmt.Sprintf("%s: the %s fix saw no CI result after %s waiting for %s (%s). Please review. %s", in.fullRepo, in.workflow, in.timeout, in.checkName, attemptsPhrase(in.attempts), changed)
		return appendFindings(text, "Targeted", in.report)
	case outcomeClean:
		return cleanText(in.workflow, in.fullRepo)
	default:
		return fmt.Sprintf("%s: the %s fix reached an unknown terminal state.", in.fullRepo, in.workflow)
	}
}

// cleanMessages are light-hearted "nothing to do" lines, rotated deterministically by repo
// name (a given repo always gets the same line — stable and testable — while different repos
// vary). The rendered line is prefixed with the capitalized workflow name (Lint, Coverage, …)
// so the relation is obvious at a glance. Kept byte-for-byte identical across all four ports
// (parity); repo names are ASCII, so the code-point sum is identical in every language.
var cleanMessages = []string{
	"nothing to see here 👏",
	"squeaky clean, no work for me 🧹",
	"all tidy already — I'll see myself out 🚪",
	"spotless, not a thing to fix 🫧",
	"already shipshape, standing down ✨",
}

// cleanText renders the clean-outcome notification body: a workflow-prefixed fun line chosen
// deterministically from cleanMessages by the repo name.
func cleanText(workflow, fullRepo string) string {
	sum := 0
	for _, r := range fullRepo {
		sum += int(r)
	}
	msg := cleanMessages[sum%len(cleanMessages)]
	title := workflow
	if title != "" {
		title = strings.ToUpper(title[:1]) + title[1:]
	}
	return fmt.Sprintf("%s: %s — %s is already clean, no PR opened.", title, msg, fullRepo)
}

func attemptsPhrase(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

// changedSummary describes the commits + files of a comparison, truncating a long file list.
func changedSummary(c githubapi.Comparison) string {
	if c.TotalCommits == 0 && len(c.Files) == 0 {
		return "No changes were recorded on the PR."
	}
	commits := "1 commit"
	if c.TotalCommits != 1 {
		commits = fmt.Sprintf("%d commits", c.TotalCommits)
	}
	return fmt.Sprintf("%s changed %s.", commits, filesPhrase(c.Files))
}

func filesPhrase(files []githubapi.ChangedFile) string {
	if len(files) == 0 {
		return "no files"
	}
	const maxFiles = 8
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Path)
	}
	suffix := ""
	if len(names) > maxFiles {
		suffix = fmt.Sprintf(" (+%d more)", len(names)-maxFiles)
		names = names[:maxFiles]
	}
	return strings.Join(names, ", ") + suffix
}

// appendFindings adds a single-line, length-bounded findings snippet to text, or returns
// text unchanged when the blob is empty.
func appendFindings(text, label, blob string) string {
	snippet := strings.Join(strings.Fields(blob), " ") // collapse newlines/whitespace
	if snippet == "" {
		return text
	}
	if r := []rune(snippet); len(r) > maxFindingsRunes {
		snippet = string(r[:maxFindingsRunes]) + "…"
	}
	return text + "\n" + label + ": " + snippet
}
