/*
 * The model-calling review stage: the category fan-out, the glue drive, diff formatting, and the
 * per-agent instruction composition.
 *
 * Returns the scorecard and the gated findings for the publish stage; posts nothing itself.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.agent.setup.driveCollectState
import com.automation.agent.agent.setup.driveText
import com.automation.agent.agent.setup.newRunner
import com.automation.agent.githubapi.PRFile
import com.google.adk.kt.agents.ParallelAgent
import com.google.adk.kt.models.Model
import java.lang.System.Logger.Level

// The user inputs that start each drive. The real instruction (lens prompt + diff) lives in the
// agents' system instruction; these just kick generation.
const val REVIEW_TRIGGER = "Review the diff and report findings as the JSON array specified."
const val GLUE_TRIGGER = "Synthesize the holistic findings as the JSON array specified."

/** The output of the model-calling stage: the scorecard and the gated findings. */
data class ReviewResult(val card: Scorecard, val findings: List<Finding>)

/**
 * Runs the model-calling stage for a reviewable PR: fan out the category lenses, run the holistic
 * glue pass, then apply the deterministic verify gate (confidence drop + dedup) and score. Returns
 * the scorecard and the gated findings (the caller publishes them).
 */
suspend fun runReview(engine: Engine, files: List<PRFile>, std: Standards?): ReviewResult {
    val diff = formatDiff(files)
    val cats = selectCategories(files)

    val category = runCategoryReview(engine, diff, cats, std)
    // Glue sees the category findings as "already reported" and skips re-flagging them, so it must
    // see only the findings that survive the same gates as the final output. Otherwise a finding the
    // verify/citation gate later drops is suppressed in glue and then dropped here, vanishing from
    // the review entirely.
    val gatedForGlue = gateCitations(engine, dropLowConfidence(category, engine.minConfidence), std)
    val glue = runGlue(engine, diff, gatedForGlue, std)

    var all = category + glue
    all = dropLowConfidence(all, engine.minConfidence) // phase-1 verify gate
    all = gateCitations(engine, all, std) // citation gate
    all = dedupe(all) // cross-lens dedup
    return ReviewResult(scoreFindings(all), all)
}

/**
 * Builds one agent per applicable category, runs them in parallel (ADK ParallelAgent — genuine
 * concurrency on the cloud backend, GPU-serialized locally with no code change), and returns every
 * category's parsed findings. Empty findings is success. The "(other)" catch-all's findings are
 * demoted to nitpick.
 */
suspend fun runCategoryReview(engine: Engine, diff: String, cats: List<Category>, std: Standards?): List<Finding> {
    val agents = cats.map { buildCategoryAgent(engine, it, diff, std) }
    val parallel = ParallelAgent(name = "review_all", description = "Per-category review in parallel", subAgents = agents)
    val state = driveCollectState(newRunner("reviewer-review", parallel), "system", "review", REVIEW_TRIGGER)

    val out = mutableListOf<Finding>()
    for (c in cats) {
        val key = findingsKey(c.name)
        if (!state.containsKey(key)) {
            // A lens that ran but found nothing is normal (empty = success); a missing state key
            // means it produced no output at all. Log it, but don't fail the whole review on one lens
            // — best-effort by design.
            engine.log.log(Level.WARNING, "category produced no findings output category=${c.name}")
        }
        val raw = state[key] as? String ?: ""
        var found = parseFindings(raw)
        if (c.other) found = demoteToNitpick(found)
        out.addAll(found)
    }
    return out
}

/**
 * Runs the holistic synthesis pass over the diff and the category findings, returning the additional
 * architectural/testability/coverage findings it produced. Empty is success.
 */
suspend fun runGlue(engine: Engine, diff: String, prior: List<Finding>, std: Standards?): List<Finding> {
    val agent = buildGlueAgent(engine, diff, prior, std)
    val text = driveText(newRunner("reviewer-glue", agent), "system", "glue", GLUE_TRIGGER)
    return parseFindings(text)
}

/**
 * Renders the filtered files as one prompt-ready diff: a header per file plus its patch in a fenced
 * block. A file with no patch (binary/oversized) is noted so the model knows it changed without a
 * hunk to review.
 */
fun formatDiff(files: List<PRFile>): String {
    val sb = StringBuilder()
    for (f in files) {
        if (f.status == "renamed" && f.previousPath.isNotEmpty()) {
            sb.append("### ${f.path} (renamed from ${f.previousPath})\n")
        } else {
            sb.append("### ${f.path} (${f.status})\n")
        }
        if (f.patch.trim().isEmpty()) {
            sb.append("(no textual diff available)\n\n")
            continue
        }
        // Patch content is untrusted (it can be a diff of a Markdown file that itself contains ```
        // runs), so pick a fence longer than the longest backtick run in the patch — otherwise an
        // embedded run would close the block early and corrupt the prompt structure.
        var fence = "`".repeat(maxBacktickRun(f.patch) + 1)
        if (fence.length < 3) fence = "```"
        sb.append(fence).append("diff\n")
        sb.append(f.patch)
        if (!f.patch.endsWith("\n")) sb.append('\n')
        sb.append(fence).append("\n\n")
    }
    return sb.toString()
}

/**
 * Returns the length of the longest run of consecutive backticks in [s] (0 if none), used to size a
 * fence that the content cannot break out of.
 */
fun maxBacktickRun(s: String): Int {
    var longest = 0
    var cur = 0
    for (ch in s) {
        if (ch == '`') {
            cur++
            if (cur > longest) longest = cur
        } else {
            cur = 0
        }
    }
    return longest
}

/** The session-state key a category agent writes its findings JSON to. */
fun findingsKey(name: String): String = "findings:$name"

/**
 * Returns the model a category runs on (code tier -> code model, else base model). Reached only from
 * the review path, which kickoff guards behind a non-null model check, so the tier model is present.
 */
fun modelForTier(engine: Engine, tier: Tier): Model {
    val model = if (tier == Tier.CODE) engine.codeLlm else engine.baseLlm
    return model ?: throw IllegalStateException("reviewer: review model not configured")
}

/**
 * Composes a category agent's instruction: the lens prompt, the repo's standards rule menu (when
 * any), and the filtered diff (baked in because they are per-event).
 */
fun buildReviewInstruction(promptBody: String, diff: String, std: Standards?): String {
    val parts = StringBuilder(promptBody)
    writeStandardsMenu(parts, std)
    parts.append("\n\n## Diff under review\n\n")
    parts.append(diff)
    return parts.toString()
}

/**
 * Composes the glue agent's instruction: the glue prompt, the standards menu, the diff, and the
 * findings the category agents already produced (so it reasons holistically without re-flagging
 * them).
 */
fun buildGlueInstruction(promptBody: String, diff: String, prior: List<Finding>, std: Standards?): String {
    val parts = StringBuilder(promptBody)
    writeStandardsMenu(parts, std)
    parts.append("\n\n## Diff under review\n\n")
    parts.append(diff)
    parts.append("\n\n## Findings already reported by other lenses\n\n")
    parts.append(findingsJson(prior))
    return parts.toString()
}

/**
 * Appends the repo's compact rule menu and the citation instruction to an agent prompt when
 * standards were discovered. The full text of any rule is available via get_rule.
 */
fun writeStandardsMenu(parts: StringBuilder, std: Standards?) {
    if (isEmpty(std)) return
    val real = std ?: return
    parts.append("\n\n## Repo standards (cite rule_id for conformance findings)\n\n")
    parts.append(real.menu())
    parts.append(
        "\nWhen a finding is a violation of one of these rules, set its dimension to the " +
            "rule's dimension and set \"rule_id\" to the rule's id. Call get_rule(id) to read a " +
            "rule's full text before flagging. Never invent a rule id; a pattern/architecture " +
            "finding with no matching rule is not a standards violation.\n",
    )
}
