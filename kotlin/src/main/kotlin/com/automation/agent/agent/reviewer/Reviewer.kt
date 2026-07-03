/*
 * The reviewer engine: dependencies, the intake pipeline (skip / deny / review), and the kickoff
 * entry point.
 *
 * Unlike the lint/coverage fixers, the reviewer is not a suspend/resume fix loop: it is mostly
 * one-shot per pull_request event and does not park on await_ci. Its long LLM compute runs in-request
 * via the execution transport (Kind.REVIEW -> /internal/dispatch), so CPU stays allocated on Cloud
 * Run.
 *
 * The flow per pull_request event: parse it, apply the trigger and skip rules, fetch the changed
 * files via the REST API, filter generated/vendored churn, and apply the two-dimensional size gate
 * to reach a decision (skip / deny / review). A review fans out the category lenses + glue pass,
 * scores the findings (count-based scorecard), and publishes a CodeRabbit-style review via REST —
 * inline comments, a marker-updated summary comment, and an advisory agent-review check —
 * reconciled against the PR's existing comments and steered off the reviewed repo's own standards
 * when present. Deny publishes the "too large, please split" summary + a neutral check.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.CheckResult
import com.automation.agent.githubapi.CheckRunInput
import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.PullRequestEvent
import com.automation.agent.githubapi.ReviewCommentRef
import com.automation.agent.githubapi.ReviewInput
import com.automation.agent.githubapi.Tree
import com.automation.agent.githubapi.parsePullRequestEvent
import com.google.adk.kt.models.Model
import java.lang.System.Logger.Level
import kotlin.coroutines.cancellation.CancellationException

/**
 * Marks branches the fixers create (they push to automation-agent/...). The reviewer skips PRs from
 * these branches so it never reviews the fixers' own PRs in a loop. Mirrors the AGENT_PR_LABEL
 * namespace.
 */
const val OWN_BRANCH_PREFIX = "automation-agent/"

/**
 * The slice of the GitHub client the reviewer needs: read the changed files (with patches), read
 * the head SHA and repo tree, and publish the review (an advisory review with inline comments, the
 * marker-updated summary comment, and the advisory agent-review check). A local interface keeps the
 * engine testable with a fake.
 */
interface GitHubClient {
    suspend fun listPRFiles(owner: String, repo: String, num: Int): List<PRFile>

    suspend fun createReview(owner: String, repo: String, num: Int, input: ReviewInput)

    suspend fun upsertMarkerComment(owner: String, repo: String, num: Int, marker: String, body: String)

    suspend fun createCheckRun(owner: String, repo: String, input: CheckRunInput)

    suspend fun listReviewComments(owner: String, repo: String, num: Int): List<ReviewCommentRef>

    suspend fun minimizeComment(subjectId: String)

    suspend fun agentCheck(owner: String, repo: String, ref: String, checkName: String): CheckResult

    suspend fun pullRequestHeadSha(owner: String, repo: String, num: Int): String

    suspend fun tree(owner: String, repo: String, ref: String): Tree

    suspend fun getFileContent(owner: String, repo: String, path: String, ref: String): String
}

/** Wires the reviewer engine. */
data class Deps(
    /**
     * The REVIEW_ENABLED kill switch. When false the engine accepts and acknowledges pull_request
     * events but does no review work — the default and the rollback posture.
     */
    val enabled: Boolean = false,
    val gh: GitHubClient? = null,
    val baseLlm: Model? = null,
    val codeLlm: Model? = null,
    /**
     * Drops findings below this confidence before scoring (the phase-1 verify gate). A non-positive
     * value keeps everything.
     */
    val minConfidence: Double = 0.0,
    /** Skips draft PRs unless the triggering action is ready_for_review. */
    val skipDrafts: Boolean = true,
    /** Drops generated/vendored/lockfile/minified/binary paths before sizing. */
    val excludeGlobs: List<String> = emptyList(),
    /** The two-dimensional size-gate caps; a non-positive value disables that dimension. */
    val maxFiles: Int = 0,
    val maxDiffBytes: Int = 0,
    /**
     * Standards-aware review settings. Carried on the engine for an easy later add (the publish +
     * standards stage); the intake/analyze stage here does not read them yet.
     */
    val standardsEnabled: Boolean = false,
    val standardsGlobs: List<String> = emptyList(),
    val standardsMaxBytes: Int = 0,
    val uncitedDrop: Boolean = false,
    val log: System.Logger? = null,
)

/** The outcome of intake for one pull_request event. */
enum class DecisionKind {
    SKIP, // not reviewable (trigger/skip rule or empty diff)
    DENY, // reviewable but too large — deny, don't degrade
    REVIEW, // proceed to review the kept files
}

/**
 * The result of the intake pipeline. files/diffBytes are the filtered review surface (set for deny
 * and review); reason explains a skip or a deny.
 */
data class Decision(
    val kind: DecisionKind,
    val reason: String,
    val files: List<PRFile>,
    val diffBytes: Int,
)

/** Runs the PR code-review workflow for one pull_request event. */
class Engine(deps: Deps) {
    val enabled: Boolean = deps.enabled

    // gh / baseLlm / codeLlm are required for real work; kickoff guards each with a controlled error
    // before any use (disabled/skip/deny paths never touch the missing one), so the collaborators can
    // treat them as always-present. gh is exposed so the sibling publish/standards modules can reach
    // it off the engine; requireGh() gives them a non-null view without the not-null assertion.
    val gh: GitHubClient? = deps.gh
    val baseLlm: Model? = deps.baseLlm
    val codeLlm: Model? = deps.codeLlm
    val minConfidence: Double = clampThreshold(deps.minConfidence)
    private val skipDrafts: Boolean = deps.skipDrafts
    private val filter: FileFilter = FileFilter(deps.excludeGlobs)
    private val maxFiles: Int = deps.maxFiles
    private val maxDiffBytes: Int = deps.maxDiffBytes
    val standardsEnabled: Boolean = deps.standardsEnabled
    val standardsGlobs: List<String> = deps.standardsGlobs
    val standardsMaxBytes: Int = deps.standardsMaxBytes
    val uncitedDrop: Boolean = deps.uncitedDrop
    val standardsCache: StandardsCache = StandardsCache()
    val log: System.Logger = deps.log ?: System.getLogger("automation-agent.reviewer")

    /** A non-null view of the GitHub client for the publish/standards paths (reached only after
     *  kickoff's client guard, so this never trips in practice). */
    fun requireGh(): GitHubClient = gh ?: throw IllegalStateException("reviewer: GitHub client not configured")

    /**
     * Handles one pull_request webhook delivery (Kind.REVIEW). The root dispatcher calls it with the
     * raw event payload; it runs in-request via the execution transport.
     *
     * When disabled (REVIEW_ENABLED=false, the default) it no-ops, so the feature is dark by default
     * and REVIEW_ENABLED is the kill switch. When enabled it runs intake and either skips, denies
     * (too large), or scores a review.
     */
    suspend fun kickoff(raw: ByteArray) {
        if (!enabled) {
            log.log(Level.DEBUG, "reviewer disabled (REVIEW_ENABLED=false); ignoring pull_request event bytes=${raw.size}")
            return
        }
        // An enabled engine needs a client to fetch the diff (both deny and review paths reach it);
        // without it, raise a controlled error rather than dereferencing a null dependency.
        val gh = this.gh ?: throw IllegalStateException("reviewer: enabled but GitHub client not configured")
        val ev =
            try {
                parsePullRequestEvent(raw)
            } catch (e: IllegalArgumentException) {
                throw IllegalArgumentException("reviewer: ${e.message}", e)
            }
        val d = decide(ev, gh)
        val pr = "${ev.repoFullName}#${ev.number}"
        // owner/repo are only used by the publish paths; decide() already validated the full name
        // before reaching a deny/review decision, so a malformed name here means skip.
        val split = splitFullName(ev.repoFullName)
        // Coalesce-to-latest: a deny/review acts on the event's SHA, so if a newer push has
        // superseded it, skip rather than post a stale review. A skip produced nothing.
        if (d.kind != DecisionKind.SKIP && superseded(split.owner, split.repo, ev, gh)) {
            log.log(Level.INFO, "stale review skipped (superseded by a newer push) pr=$pr eventSha=${ev.headSha}")
            return
        }
        val meta =
            PublishMeta(
                owner = split.owner,
                repo = split.repo,
                number = ev.number,
                headSha = ev.headSha,
                files = d.files,
                tiers = "code-reasoning + base",
            )
        when (d.kind) {
            DecisionKind.SKIP ->
                log.log(Level.INFO, "review skipped pr=$pr action=${ev.action} reason=${d.reason}")
            DecisionKind.DENY -> {
                // Too large to review: post the "please split" summary + a neutral check, no model call.
                publishDeny(this, meta, d.reason, d.files.size, d.diffBytes)
                log.log(Level.INFO, "review denied pr=$pr files=${d.files.size} diffBytes=${d.diffBytes} reason=${d.reason}")
            }
            DecisionKind.REVIEW -> {
                // Review needs both tier models; the deny branch above does not.
                if (baseLlm == null || codeLlm == null) {
                    throw IllegalStateException("reviewer: enabled but review models not configured")
                }
                // Steer the lenses off the reviewed repo's own conventions; null when disabled or none
                // found, in which case the lenses review generically.
                val std = discoverStandards(this, split.owner, split.repo, ev.headSha, d.files)
                meta.standards = std?.sourceList() ?: emptyList()
                val result = runReview(this, d.files, std)
                publish(this, result.card, result.findings, meta)
                log.log(
                    Level.INFO,
                    "review published pr=$pr files=${d.files.size} overall=${levelGlyph(result.card.overall)} findings=${result.card.total}",
                )
            }
        }
    }

    /**
     * Runs the deterministic intake pipeline for one event: trigger gate -> skip rules -> fetch files
     * -> filter -> size gate. It performs no model calls and posts nothing.
     */
    suspend fun decide(ev: PullRequestEvent, gh: GitHubClient): Decision {
        if (ev.action !in TRIGGER_ACTIONS) {
            return skip("action \"${ev.action}\" is not a reviewed trigger")
        }
        if (skipDrafts && ev.draft && ev.action != "ready_for_review") {
            return skip("draft PR (REVIEW_SKIP_DRAFTS)")
        }
        if (ev.headRef.startsWith(OWN_BRANCH_PREFIX)) {
            return skip("agent's own PR (head \"${ev.headRef}\")")
        }
        if (ev.labels.contains("skip-review")) {
            return skip("skip-review label")
        }
        if (isDependencyBot(ev.authorLogin)) {
            return skip("dependency-bot PR (${ev.authorLogin})")
        }

        val split = splitFullName(ev.repoFullName)
        if (!split.ok) {
            throw IllegalArgumentException("reviewer: malformed repository full name \"${ev.repoFullName}\"")
        }
        val files =
            try {
                gh.listPRFiles(split.owner, split.repo, ev.number)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                throw RuntimeException("reviewer: list PR files: ${e.message}", e)
            }
        val filtered = filter.apply(files)
        if (filtered.kept.isEmpty()) {
            return skip("no reviewable files after exclude filter (${files.size} changed)")
        }
        val gate = oversize(filtered.kept.size, filtered.diffBytes, maxFiles, maxDiffBytes)
        if (gate.denied) {
            return Decision(DecisionKind.DENY, gate.reason, filtered.kept, filtered.diffBytes)
        }
        return Decision(DecisionKind.REVIEW, "", filtered.kept, filtered.diffBytes)
    }

    /**
     * Reports whether a newer push has replaced the SHA this task was enqueued for. It is
     * best-effort: a missing event SHA or a lookup error yields false (proceed) so a transient
     * failure never suppresses a real review.
     */
    private suspend fun superseded(owner: String, repo: String, ev: PullRequestEvent, gh: GitHubClient): Boolean {
        if (ev.headSha.isEmpty()) return false
        val current =
            try {
                gh.pullRequestHeadSha(owner, repo, ev.number)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                log.log(Level.WARNING, "could not fetch current head SHA; proceeding with review pr=${ev.repoFullName} err=${e.message}")
                return false
            }
        return current.isNotEmpty() && current != ev.headSha
    }
}

/** Builds the reviewer engine from its dependencies. */
fun newEngine(deps: Deps): Engine = Engine(deps)

// The pull_request actions that open a review.
private val TRIGGER_ACTIONS = setOf("opened", "reopened", "synchronize", "ready_for_review")

/** Builds a skip decision with a formatted reason. */
private fun skip(reason: String): Decision = Decision(DecisionKind.SKIP, reason, emptyList(), 0)

/** Reports whether the author is a known dependency-update bot. GitHub Apps post as "<name>[bot]". */
fun isDependencyBot(login: String): Boolean = login == "dependabot[bot]" || login == "renovate[bot]"

/** The result of splitting an "owner/name" full repository name. */
data class SplitName(val owner: String, val repo: String, val ok: Boolean)

/**
 * Splits an "owner/name" repository full name. Reports ok=false for anything that is not exactly one
 * owner and one non-empty name.
 */
fun splitFullName(full: String): SplitName {
    val idx = full.indexOf('/')
    if (idx < 0) return SplitName("", "", false)
    val owner = full.substring(0, idx)
    val repo = full.substring(idx + 1)
    if (owner.isEmpty() || repo.isEmpty() || repo.contains('/')) return SplitName("", "", false)
    return SplitName(owner, repo, true)
}
