/*
 * Package config loads the automation-agent runtime configuration from the
 * environment. It is the single source of truth for settings; no other package
 * should read the environment directly. See ../.agents/standards/architecture-design.md §12.
 */
package io.github.jkjamies.automationagent.config

import kotlin.time.Duration
import kotlin.time.Duration.Companion.hours
import kotlin.time.Duration.Companion.microseconds
import kotlin.time.Duration.Companion.milliseconds
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.nanoseconds
import kotlin.time.Duration.Companion.seconds

/** Provider selects which LLM backend agents use. */
enum class Provider(val value: String) {
    OLLAMA("ollama"),
    GEMINI("gemini"),
    ;

    override fun toString(): String = value

    companion object {
        fun from(s: String): Provider? = entries.firstOrNull { it.value == s }
    }
}

/** NotifyProvider selects where summaries are posted. */
enum class NotifyProvider(val value: String) {
    SLACK("slack"),
    TEAMS("teams"),
    ;

    override fun toString(): String = value

    companion object {
        fun from(s: String): NotifyProvider? = entries.firstOrNull { it.value == s }
    }
}

/**
 * SessionBackend selects where the ADK session (the durable suspend/resume history of a parked
 * fix run) and its park record live.
 */
enum class SessionBackend(val value: String) {
    /** In-process: tests and ephemeral local runs. A restart strands parked runs. */
    MEMORY("memory"),

    /** Local file via a hand-rolled sqlite session service, so a parked run survives a restart. */
    SQLITE("sqlite"),

    /** Cloud backend (serverless, scales to zero): a hand-rolled Firestore session service + park store. */
    FIRESTORE("firestore"),
    ;

    override fun toString(): String = value

    companion object {
        fun from(s: String): SessionBackend? = entries.firstOrNull { it.value == s }
    }
}

/** Config holds all runtime settings. */
data class Config(
    // LLM
    val llmProvider: Provider,
    val ollamaHost: String,
    val ollamaModel: String, // default model: triage, explore, summary
    val geminiModel: String,
    // Code model: the (typically larger) model used for code-change steps
    // (lint rewrite, coverage test generation). Falls back to the default model.
    val ollamaCodeModel: String,
    val geminiCodeModel: String,
    // GitHub / repos
    val repos: List<String>,
    val githubToken: String,
    // Notifications
    val notifyProvider: NotifyProvider,
    val slackWebhookUrl: String,
    val teamsWebhookUrl: String,
    // Server / schedule
    val port: String,
    val cronDaily: String,
    val cronWeekly: String,
    // Lint-fixer
    val maxIterations: Int,
    // ciTimeout bounds how long a suspended fix run waits for its CI result before it is
    // resumed with a timeout outcome (notify + stop). Per-run timer, not a scan.
    val ciTimeout: Duration,
    val githubWebhookSecret: String,
    // Single human-facing label applied to every agent PR on creation (AGENT_PR_LABEL).
    // Write-only: PR lookup is by branch, so the label never gates behavior.
    val agentPrLabel: String,
    // Sessions: where the durable suspend/resume session + park record live.
    val sessionBackend: SessionBackend,
    // sqliteDsn is the database path for SESSION_BACKEND=sqlite (ignored otherwise).
    val sqliteDsn: String,
    // firestoreProject is the GCP project for SESSION_BACKEND=firestore; empty detects it from
    // ADC / GOOGLE_CLOUD_PROJECT. firestoreCollection is the collection-name prefix.
    val firestoreProject: String,
    val firestoreCollection: String,
    // internalToken is the Bearer token guarding the /internal/* endpoints (Cloud Scheduler
    // cron + sweep). Empty disables those routes (they 404).
    val internalToken: String,
) {
    /**
     * Checks invariants that the type system alone cannot guarantee. Provider and notify
     * validity are enforced when the config is loaded (invalid values fail [loadFrom]); this
     * covers the remaining numeric invariants (MAX_ITERATIONS, PORT).
     */
    fun validate() {
        require(maxIterations >= 1) { "MAX_ITERATIONS must be >= 1, got $maxIterations" }
        val portNum = port.toIntOrNull()
            ?: throw IllegalArgumentException("PORT must be numeric, got \"$port\"")
        require(portNum in 1..65535) { "PORT must be in 1..65535, got $portNum" }
    }

    companion object {
        /** A function that resolves an environment key to its value, or null if unset. */
        fun interface Lookup {
            operator fun invoke(key: String): String?
        }

        /** Load reads configuration from the process environment, applying defaults. */
        fun load(): Config {
            val c = loadFrom { key -> System.getenv(key) }
            // When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's
            // gh CLI login so a local run authenticates to GitHub without a hand-set token.
            return if (c.githubToken.isEmpty()) c.copy(githubToken = ghCliToken()) else c
        }

        /**
         * loadFrom builds a Config from an arbitrary lookup, which keeps [load] testable
         * without mutating the real environment.
         */
        fun loadFrom(get: Lookup): Config {
            val llmProviderRaw = getOr(get, "LLM_PROVIDER", Provider.OLLAMA.value)
            val llmProvider = Provider.from(llmProviderRaw)
                ?: throw IllegalArgumentException("invalid LLM_PROVIDER \"$llmProviderRaw\" (want ollama|gemini)")

            val notifyProviderRaw = getOr(get, "NOTIFY_PROVIDER", NotifyProvider.SLACK.value)
            val notifyProvider = NotifyProvider.from(notifyProviderRaw)
                ?: throw IllegalArgumentException("invalid NOTIFY_PROVIDER \"$notifyProviderRaw\" (want slack|teams)")

            val maxIterationsRaw = getOr(get, "MAX_ITERATIONS", "3")
            val maxIterations = maxIterationsRaw.toIntOrNull()
                ?: throw IllegalArgumentException("MAX_ITERATIONS: invalid integer \"$maxIterationsRaw\"")

            val ciTimeoutRaw = getOr(get, "CI_TIMEOUT", "90m")
            val ciTimeout = parseGoDuration(ciTimeoutRaw)
                ?: throw IllegalArgumentException("CI_TIMEOUT: invalid duration \"$ciTimeoutRaw\"")

            val ollamaModel = getOr(get, "OLLAMA_MODEL", "gemma4:12b")
            val geminiModel = getOr(get, "GEMINI_MODEL", "")
            // Code-change steps use the larger 26b model by default; the Gemini code model
            // still falls back to its base model when unset.
            val ollamaCodeModel = getOr(get, "OLLAMA_CODE_MODEL", "gemma4:26b")
            val geminiCodeModel = getOr(get, "GEMINI_CODE_MODEL", "").ifEmpty { geminiModel }

            val sessionBackendRaw = getOr(get, "SESSION_BACKEND", SessionBackend.MEMORY.value)
            val sessionBackend = SessionBackend.from(sessionBackendRaw)
                ?: throw IllegalArgumentException("invalid SESSION_BACKEND \"$sessionBackendRaw\" (want memory|sqlite|firestore)")

            val c = Config(
                llmProvider = llmProvider,
                ollamaHost = getOr(get, "OLLAMA_HOST", "http://localhost:11434"),
                ollamaModel = ollamaModel,
                geminiModel = geminiModel,
                ollamaCodeModel = ollamaCodeModel,
                geminiCodeModel = geminiCodeModel,
                repos = splitList(getOr(get, "REPOS", "")),
                githubToken = getOr(get, "GITHUB_TOKEN", getOr(get, "GH_TOKEN", "")),
                notifyProvider = notifyProvider,
                slackWebhookUrl = getOr(get, "SLACK_WEBHOOK_URL", ""),
                teamsWebhookUrl = getOr(get, "TEAMS_WEBHOOK_URL", ""),
                port = getOr(get, "PORT", "8080"),
                cronDaily = getOr(get, "CRON_DAILY", "0 9 * * *"),
                cronWeekly = getOr(get, "CRON_WEEKLY", "0 9 * * 1"),
                maxIterations = maxIterations,
                ciTimeout = ciTimeout,
                githubWebhookSecret = getOr(get, "GITHUB_WEBHOOK_SECRET", ""),
                agentPrLabel = getOr(get, "AGENT_PR_LABEL", "automation-agent"),
                sessionBackend = sessionBackend,
                sqliteDsn = getOr(get, "SQLITE_DSN", "automation-agent.db"),
                firestoreProject = getOr(get, "FIRESTORE_PROJECT", ""),
                firestoreCollection = getOr(get, "FIRESTORE_COLLECTION", "automation_agent"),
                internalToken = getOr(get, "INTERNAL_TOKEN", ""),
            )
            c.validate()
            return c
        }
    }
}

private fun getOr(get: Config.Companion.Lookup, key: String, def: String): String {
    val v = get(key)
    return if (!v.isNullOrEmpty()) v else def
}

/**
 * Returns the token from `gh auth token`, or "" if the gh CLI is missing, unauthenticated, or
 * errors. This is the one place config shells out rather than reading the environment; it exists
 * so local runs reuse an existing gh login. The short timeout guards against a hung subprocess
 * stalling startup.
 */
private fun ghCliToken(): String {
    return try {
        val proc = ProcessBuilder("gh", "auth", "token")
            .redirectErrorStream(false)
            .start()
        val out = proc.inputStream.bufferedReader().use { it.readText() }.trim()
        val finished = proc.waitFor(5, java.util.concurrent.TimeUnit.SECONDS)
        if (!finished) {
            proc.destroy()
            ""
        } else if (proc.exitValue() == 0) {
            out
        } else {
            ""
        }
    } catch (_: java.io.IOException) {
        ""
    } catch (_: InterruptedException) {
        ""
    }
}

private fun splitList(s: String): List<String> {
    if (s.isBlank()) return emptyList()
    return s.split(",").map { it.trim() }.filter { it.isNotEmpty() }
}

/**
 * Parses a duration string (e.g. "90m", "1h30m", "500ms") into a [Duration], returning null on
 * malformed input. Supports the unit subset the service uses (ns, us/µs, ms, s, m, h). A bare "0"
 * is the zero duration.
 */
private fun parseGoDuration(s: String): Duration? {
    if (s.isEmpty()) return null
    if (s == "0") return Duration.ZERO

    var i = 0
    var total = Duration.ZERO
    var sawSegment = false
    while (i < s.length) {
        val start = i
        while (i < s.length && (s[i].isDigit() || s[i] == '.')) i++
        if (i == start) return null // expected a number
        val value = s.substring(start, i).toDoubleOrNull() ?: return null

        val unitStart = i
        while (i < s.length && !s[i].isDigit() && s[i] != '.') i++
        val unit = s.substring(unitStart, i)
        val segment = when (unit) {
            "ns" -> value.nanoseconds
            "us", "µs", "μs" -> value.microseconds
            "ms" -> value.milliseconds
            "s" -> value.seconds
            "m" -> value.minutes
            "h" -> value.hours
            else -> return null
        }
        total += segment
        sawSegment = true
    }
    return if (sawSegment) total else null
}
