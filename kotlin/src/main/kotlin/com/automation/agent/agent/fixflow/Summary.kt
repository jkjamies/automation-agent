package com.automation.agent.agent.fixflow

import com.automation.agent.githubapi.ChangedFile
import com.automation.agent.githubapi.Comparison

/** The way a fix run ended; it selects the summary framing. */
enum class TerminalOutcome { SUCCESS, EXHAUSTED, TIMEOUT, CLEAN }

/**
 * Everything a terminal summary needs. The per-attempt work product lives only in the PR (commits +
 * diff), never the session, so [changed] (a base...head comparison) is how the human learns what the
 * agent actually did.
 */
data class SummaryInput(
    val outcome: TerminalOutcome,
    val workflow: String, // spec.name (lint | coverage)
    val fullRepo: String,
    val prNumber: Int,
    val attempts: Int,
    val report: String = "", // original targeted findings (RunParams.report)
    val lastOutput: String = "", // last CI check output (exhausted) — the remaining findings
    val timeout: String = "", // CI timeout (timeout outcome)
    val checkName: String = "", // the awaited check (timeout outcome)
    val changed: Comparison = Comparison(),
)

/** Bounds how much of a (potentially large) findings blob a summary inlines. */
private const val MAX_FINDINGS_RUNES = 280
private const val MAX_FILES = 8

/**
 * Frames a terminal outcome into a human notification body, enriched with the original findings and
 * what changed on the PR. Pure (no I/O) so it is unit-testable.
 */
fun buildSummaryText(input: SummaryInput): String {
    val changed = changedSummary(input.changed)
    return when (input.outcome) {
        TerminalOutcome.SUCCESS ->
            appendFindings("${input.fullRepo}: the ${input.workflow} fix passed CI after ${attemptsPhrase(input.attempts)}. $changed", "Targeted", input.report)
        TerminalOutcome.EXHAUSTED ->
            appendFindings("${input.fullRepo}: the ${input.workflow} fix still fails CI after ${attemptsPhrase(input.attempts)}. Please review. $changed", "Remaining", input.lastOutput)
        TerminalOutcome.TIMEOUT ->
            appendFindings("${input.fullRepo}: the ${input.workflow} fix saw no CI result after ${input.timeout} waiting for ${input.checkName} (${attemptsPhrase(input.attempts)}). Please review. $changed", "Targeted", input.report)
        TerminalOutcome.CLEAN ->
            cleanText(input.workflow, input.fullRepo)
    }
}

// Light-hearted "nothing to do" lines, rotated deterministically by repo name (a given repo
// always gets the same line — stable and testable — while different repos vary). The rendered
// line is prefixed with the capitalized workflow name (Lint, Coverage, …) so the relation is
// obvious at a glance. Kept byte-for-byte identical across all four ports (parity); repo names
// are ASCII, so the code-point sum is identical in every language.
private val cleanMessages = listOf(
    "nothing to see here 👏",
    "squeaky clean, no work for me 🧹",
    "all tidy already — I'll see myself out 🚪",
    "spotless, not a thing to fix 🫧",
    "already shipshape, standing down ✨",
)

// cleanText renders the clean-outcome body: a workflow-prefixed fun line chosen
// deterministically from cleanMessages by the repo name.
private fun cleanText(workflow: String, fullRepo: String): String {
    val msg = cleanMessages[fullRepo.sumOf { it.code } % cleanMessages.size]
    val title = workflow.replaceFirstChar { it.uppercase() }
    return "$title: $msg — $fullRepo is already clean, no PR opened."
}

private fun attemptsPhrase(n: Int): String = if (n == 1) "1 attempt" else "$n attempts"

/** Describes the commits + files of a comparison, truncating a long file list. */
private fun changedSummary(c: Comparison): String {
    if (c.totalCommits == 0 && c.files.isEmpty()) return "No changes were recorded on the PR."
    val commits = if (c.totalCommits == 1) "1 commit" else "${c.totalCommits} commits"
    return "$commits changed ${filesPhrase(c.files)}."
}

private fun filesPhrase(files: List<ChangedFile>): String {
    if (files.isEmpty()) return "no files"
    val names = files.map { it.path }
    return if (names.size > MAX_FILES) {
        names.take(MAX_FILES).joinToString(", ") + " (+${names.size - MAX_FILES} more)"
    } else {
        names.joinToString(", ")
    }
}

/**
 * Adds a single-line, length-bounded findings snippet to [text], or returns [text] unchanged when
 * the blob is empty.
 */
private fun appendFindings(text: String, label: String, blob: String): String {
    val snippet = blob.split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ") // collapse whitespace
    if (snippet.isEmpty()) return text
    val bounded = if (snippet.length > MAX_FINDINGS_RUNES) snippet.take(MAX_FINDINGS_RUNES) + "…" else snippet
    return "$text\n$label: $bounded"
}
