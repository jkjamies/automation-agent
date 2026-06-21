/*
 * Package fixflow is the reusable engine behind the PR-fixing agents (lint-fixer, coverage-fixer,
 * …). It owns the event-driven loop — kickoff → suspend → CI resume → loop or finish — plus the
 * apply mechanics and attempt counting. Each concrete agent supplies a Spec (a triage fn, an
 * analyze fn, and its branch/label/check names). State lives on GitHub; there is no local store.
 */
package io.github.jkjamies.automationagent.agent.fixflow

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

internal val fixflowJson = Json { ignoreUnknownKeys = true }

/**
 * The trusted envelope a CI job posts: [repo]/[base] identify where to work (reliable), and
 * [report] is the arbitrary tool output (lint report, coverage report, …) the agent's triage LLM
 * reasons over.
 */
data class Kickoff(
    val repo: String,
    val base: String,
    val report: kotlinx.serialization.json.JsonElement,
) {
    /**
     * The report as clean text for the LLM. A JSON-string report (lcov/JaCoCo text wrapped in a
     * JSON string) is unquoted so the model sees the raw report; a JSON-value report is passed
     * through as compact JSON.
     */
    fun reportText(): String = if (report is JsonPrimitive && report.isString) report.content else report.toString()

    /** The owner portion of [repo]. */
    fun owner(): String = splitRepo(repo)?.first ?: ""

    /** The repository-name portion of [repo]. */
    fun name(): String = splitRepo(repo)?.second ?: ""
}

/** Parses and validates the envelope, applying defaults. */
fun parseKickoff(raw: ByteArray): Kickoff = parseKickoff(String(raw, Charsets.UTF_8))

fun parseKickoff(raw: String): Kickoff {
    val element =
        try {
            fixflowJson.parseToJsonElement(raw)
        } catch (e: Exception) {
            throw IllegalArgumentException("parse kickoff: ${e.message}")
        }
    val obj = element as? JsonObject ?: throw IllegalArgumentException("parse kickoff: not a JSON object")

    val repoPrim = obj["repo"] as? JsonPrimitive
    val repo = repoPrim?.takeIf { it.isString }?.content?.trim().orEmpty()
    require(repo.isNotEmpty()) { "kickoff: repo is required" }
    require(splitRepo(repo) != null) { "kickoff: repo \"$repo\" must be owner/name" }

    val report = obj["report"] ?: throw IllegalArgumentException("kickoff: report is required")

    val base = (obj["base"] as? JsonPrimitive)?.takeIf { it.isString }?.content?.takeIf { it.isNotEmpty() } ?: "main"
    return Kickoff(repo = repo, base = base, report = report)
}

/** Splits "owner/repo" into its parts, or null if malformed. */
internal fun splitRepo(s: String): Pair<String, String>? {
    val owner = s.substringBefore('/', "")
    val repo = s.substringAfter('/', "")
    return if (!s.contains('/') || owner.isEmpty() || repo.isEmpty()) null else owner to repo
}
