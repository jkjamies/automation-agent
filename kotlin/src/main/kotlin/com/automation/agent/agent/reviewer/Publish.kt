/*
 * The CodeRabbit-style publish stage: assembly + REST writes (advisory review, marker summary
 * comment, advisory agent-review check), reconciled against the PR's existing fingerprinted
 * comments. Nothing here gates a merge.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.CheckRunInput
import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.ReviewComment
import com.automation.agent.githubapi.ReviewInput


// The advisory check the reviewer publishes (agent-published, human-consumed). Globally unique and
// identical across ports (external contract).
const val CHECK_NAME = "agent-review"

/** Carries the per-PR identifiers and context the published artifacts need. */
data class PublishMeta(
    val owner: String,
    val repo: String,
    val number: Int,
    val headSha: String,
    val files: List<PRFile> = emptyList(), // for the in-diff index
    val tiers: String = "", // model tiers used, for the Review details section
    var standards: List<String> = emptyList(), // applied source paths (empty = generic)
)

/**
 * The hidden HTML comment that identifies the reviewer's single summary comment so a re-review
 * updates it rather than posting a new one.
 */
fun summaryMarker(owner: String, repo: String, number: Int): String = "<!-- automation-agent:review:$owner/$repo#$number -->"

/**
 * Posts the review for a scored PR: inline comments for in-diff actionable findings, a
 * marker-updated summary comment with the scorecard, and the advisory agent-review check.
 * Out-of-diff actionable findings and nitpicks go into the summary (never dropped).
 */
suspend fun publish(engine: Engine, card: Scorecard, findings: List<Finding>, meta: PublishMeta) {
    val gh = engine.requireGh()
    // At-least-once safety: reconciliation makes the inline comments idempotent, but the check run and
    // summary are create/upsert-only, so a redelivered task for a SHA already published would
    // duplicate the check. If the agent-review check already exists for this head SHA, skip — a
    // genuine re-push carries a new SHA and still reconciles below.
    if (alreadyPublished(engine, meta)) {
        engine.log.log(System.Logger.Level.INFO, "review already published for head SHA; skipping re-post repo=${meta.owner}/${meta.repo} sha=${meta.headSha}")
        return
    }
    val idx = DiffIndex(meta.files)
    val (inline, outOfDiff, nitpicks) = classify(findings, idx)
    val actionable = inline.size + outOfDiff.size

    // Reconcile against the comments already on the PR (GitHub-as-store): keep inline findings that
    // still apply (don't re-post — idempotent), post only new ones, and minimize the comments whose
    // finding is gone.
    val existing = gh.listReviewComments(meta.owner, meta.repo, meta.number)
    val rec = reconcile(inline, existing)

    // Post only the new inline findings; an empty review is noise.
    if (rec.toPost.isNotEmpty()) {
        val comments = rec.toPost.map { f -> ReviewComment(path = f.file, line = f.line, side = "RIGHT", body = inlineCommentBody(f)) }
        val body = "${levelGlyph(card.overall)} Agent review — see the summary comment for the full scorecard."
        gh.createReview(meta.owner, meta.repo, meta.number, ReviewInput(body = body, comments = comments))
    }

    // Minimize the comments whose finding no longer applies — best-effort. New inline comments are
    // already posted but the summary and check are not; aborting here on a single minimize failure
    // would leave the PR without its summary/check. So log and continue per node.
    for (nodeId in rec.toMinimize) {
        try {
            gh.minimizeComment(nodeId)
        } catch (e: Exception) {
            engine.log.log(System.Logger.Level.WARNING, "reviewer: minimize outdated comment failed; continuing repo=${meta.owner}/${meta.repo} node=$nodeId err=${e.message}")
        }
    }

    val marker = summaryMarker(meta.owner, meta.repo, meta.number)
    gh.upsertMarkerComment(meta.owner, meta.repo, meta.number, marker, summaryComment(marker, card, actionable, nitpicks, outOfDiff, meta))

    gh.createCheckRun(
        meta.owner,
        meta.repo,
        CheckRunInput(
            name = CHECK_NAME,
            headSha = meta.headSha,
            conclusion = checkConclusion(card.overall),
            title = "${levelGlyph(card.overall)} Agent review — ${levelWord(card.overall)}",
            summary = "Overall: ${levelWord(card.overall)} · Actionable comments: $actionable",
        ),
    )
}

/**
 * Posts the "too large to review" outcome: a marker-updated summary comment framed fail-like (🔴)
 * plus a neutral check. No model call was made.
 */
suspend fun publishDeny(engine: Engine, meta: PublishMeta, reason: String, files: Int, diffBytes: Int) {
    val gh = engine.requireGh()
    if (alreadyPublished(engine, meta)) {
        engine.log.log(System.Logger.Level.INFO, "deny already published for head SHA; skipping re-post repo=${meta.owner}/${meta.repo} sha=${meta.headSha}")
        return
    }
    val marker = summaryMarker(meta.owner, meta.repo, meta.number)
    val body =
        "$marker\n## 🔴 Agent review — too large for automated review\n\n" +
            "This PR is too large to review automatically ($files files / $diffBytes bytes " +
            "after excluding generated files). Please split it into smaller PRs.\n\n" +
            "_${reason}_\n"
    gh.upsertMarkerComment(meta.owner, meta.repo, meta.number, marker, body)
    gh.createCheckRun(
        meta.owner,
        meta.repo,
        CheckRunInput(
            name = CHECK_NAME,
            headSha = meta.headSha,
            conclusion = "neutral",
            title = "🔴 Agent review — too large",
            summary = "$files files / $diffBytes bytes after excluding generated files; please split.",
        ),
    )
}

/**
 * Reports whether the agent-review check already exists for the head SHA. A lookup error is treated
 * as "not published" so a transient failure never suppresses a real review.
 */
suspend fun alreadyPublished(engine: Engine, meta: PublishMeta): Boolean =
    try {
        engine.requireGh().agentCheck(meta.owner, meta.repo, meta.headSha, CHECK_NAME).found
    } catch (_: Exception) {
        false
    }

/** The three-way split of confidence-gated findings the publish stage routes on. */
data class Classified(val inline: List<Finding>, val outOfDiff: List<Finding>, val nitpicks: List<Finding>)

/**
 * Splits confidence-gated findings into inline findings (actionable, on a commentable diff line),
 * out-of-diff actionable findings (listed in the summary, never snapped to a wrong line), and
 * nitpicks (collapsed in the summary).
 */
fun classify(findings: List<Finding>, idx: DiffIndex): Classified {
    val inline = mutableListOf<Finding>()
    val outOfDiff = mutableListOf<Finding>()
    val nitpicks = mutableListOf<Finding>()
    for (f in findings) {
        when {
            f.severity == Severity.NITPICK -> nitpicks.add(f)
            f.file != "" && f.line > 0 && idx.inDiff(f.file, f.line) -> inline.add(f)
            else -> outOfDiff.add(f)
        }
    }
    return Classified(inline = inline, outOfDiff = outOfDiff, nitpicks = nitpicks)
}

/**
 * Renders one inline comment: an icon+category prefix, the message, an optional ```suggestion block
 * (a localized fix), and an optional "Prompt for AI agents" block.
 */
fun inlineCommentBody(f: Finding): String {
    val parts = StringBuilder()
    // Dimension/severity are normalized to known enums, so only the model-authored message needs
    // sanitizing here.
    parts.append("**${findingPrefix(f)}** · _${f.dimension.value}_\n\n${sanitizeText(f.message)}\n")
    if (f.suggestion != "") {
        // Suggestion is model-authored; size the outer fence past any backtick run in it so a suggestion
        // containing a ```fence can't close the block early and inject markdown or @mentions.
        var fence = "`".repeat(maxBacktickRun(f.suggestion) + 1)
        if (fence.length < 3) fence = "```"
        parts.append("\n").append(fence).append("suggestion\n")
        parts.append(f.suggestion)
        if (!f.suggestion.endsWith("\n")) parts.append("\n")
        parts.append(fence).append("\n")
    }
    if (f.fixPrompt != "") {
        // FixPrompt is model-authored; render it inside a code fence so any @mentions or HTML are
        // literal (not pinged/injected) and it stays copy-pasteable.
        var fence = "`".repeat(maxBacktickRun(f.fixPrompt) + 1)
        if (fence.length < 3) fence = "```"
        parts.append("\n<details>\n<summary>🤖 Prompt for AI agents</summary>\n\n")
        parts.append(fence).append("\n")
        parts.append(f.fixPrompt)
        if (!f.fixPrompt.endsWith("\n")) parts.append("\n")
        parts.append(fence).append("\n\n</details>\n")
    }
    // Hidden fingerprint marker so a later re-review re-identifies this comment and reconciles it.
    parts.append("\n").append(fpMarker(fingerprint(f))).append("\n")
    return parts.toString()
}

// Matches an @ immediately followed by a mention character; sanitizeText inserts a zero-width space
// after the @ so GitHub does not render (and notify) it as a mention.
private val MENTION_PATTERN = Regex("@([A-Za-z0-9])")

/**
 * Neutralizes model-authored text for safe embedding in a Markdown comment: escape HTML-significant
 * characters (so a finding can't inject markup such as </details>) and break @mentions with a
 * zero-width space (so the reviewer never pings a real user). Code in ```suggestion blocks and
 * fenced FixPrompt is left untouched by callers.
 */
fun sanitizeText(s: String): String {
    var out = s.replace("&", "&amp;")
    out = out.replace("<", "&lt;")
    out = out.replace(">", "&gt;")
    return MENTION_PATTERN.replace(out, "@​$1")
}

/** The icon+category label that leads an inline comment. */
fun findingPrefix(f: Finding): String {
    if (f.dimension == Dimension.SECURITY) return "🔒 Security"
    if (f.severity == Severity.CRITICAL || f.severity == Severity.MAJOR) return "⚠️ Potential issue"
    return "🛠️ Refactor"
}

/**
 * Assembles the marker-updated summary comment: header, scorecard table, and collapsible sections
 * for nitpicks, out-of-diff findings, and review details.
 */
fun summaryComment(marker: String, card: Scorecard, actionable: Int, nitpicks: List<Finding>, outOfDiff: List<Finding>, meta: PublishMeta): String {
    val parts = StringBuilder()
    parts.append(marker).append("\n")
    parts.append("## ${levelGlyph(card.overall)} Agent review — Overall: ${levelWord(card.overall)} · Actionable comments: $actionable\n\n")
    parts.append(scorecardTable(card))
    if (nitpicks.isNotEmpty()) parts.append(collapsible("🧹 Nitpicks (${nitpicks.size})", findingsList(nitpicks)))
    if (outOfDiff.isNotEmpty()) parts.append(collapsible("🔭 Outside diff range (${outOfDiff.size})", findingsList(outOfDiff)))
    parts.append(collapsible("Review details", reviewDetails(meta)))
    return parts.toString()
}

/**
 * Renders the per-dimension severity histogram. With no findings it states so rather than emitting
 * an empty table.
 */
fun scorecardTable(card: Scorecard): String {
    if (card.dims.isEmpty()) return "_No findings._\n\n"
    val parts = StringBuilder()
    parts.append("| Dimension | Level | Critical | Major | Medium | Nitpick |\n")
    parts.append("|---|---|---|---|---|---|\n")
    for (d in card.dims) {
        parts.append("| ${d.dimension.value} | ${levelGlyph(d.level)} | ${d.critical} | ${d.major} | ${d.medium} | ${d.nitpick} |\n")
    }
    parts.append("\n")
    return parts.toString()
}

/** Renders findings as a bulleted file:line list for the summary's collapsible sections. */
fun findingsList(fs: List<Finding>): String {
    val parts = StringBuilder()
    for (f in fs) {
        val loc = if (f.line > 0) "${f.file}:${f.line}" else f.file
        parts.append("- **${f.severity.value}** `$loc` _(${f.dimension.value})_ — ${sanitizeText(f.message)}\n")
    }
    return parts.toString()
}

/** Renders the "Review details" section: head SHA, file count, and the model tiers. */
fun reviewDetails(meta: PublishMeta): String {
    val parts = StringBuilder()
    parts.append("- Head SHA: `${meta.headSha}`\n")
    parts.append("- Files reviewed: ${meta.files.size}\n")
    if (meta.tiers != "") parts.append("- Model tiers: ${meta.tiers}\n")
    if (meta.standards.isNotEmpty()) {
        parts.append("- Standards applied: ${meta.standards.joinToString(", ")}\n")
    } else {
        // Empty also covers standards-off and the discovery/distill fallback, not just a repo with no
        // convention docs — so stay neutral rather than asserting none were found.
        parts.append("- Standards: generic review\n")
    }
    return parts.toString()
}

/** Wraps [body] in a <details> block with the given summary label. */
fun collapsible(summary: String, body: String): String = "\n<details>\n<summary>$summary</summary>\n\n$body\n</details>\n"

/**
 * Maps the overall grade to the advisory check conclusion: green is success; yellow and red are
 * neutral. It is never failure — the reviewer never gates a merge.
 */
fun checkConclusion(overall: Level): String = if (overall == Level.GREEN) "success" else "neutral"
