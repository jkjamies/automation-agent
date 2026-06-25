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
	default:
		return fmt.Sprintf("%s: the %s fix reached an unknown terminal state.", in.fullRepo, in.workflow)
	}
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
