/*
 * Package config loads the automation-agent runtime configuration from the
 * environment. It is the single source of truth for settings; no other package
 * should read the environment directly. See ../.agents/standards/architecture-design.md §12.
 */
package com.automation.agent.config

import com.automation.agent.auth.parseRsaPrivateKey
import java.io.File
import java.io.IOException
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
    // GitHub App credentials (production auth path). [githubAppId] == 0 means App mode is off and the
    // static [githubToken] (PAT) is used. Resolved at load time; partial/misconfigured App vars are a
    // startup error, never a silent fallback. See [appMode] and
    // specs/20260625-github-app-authentication.md.
    val githubAppId: Long,
    val githubAppInstallationId: Long,
    // The App private key in PEM form, already unescaped and validated to parse as RSA (the literal
    // bytes from GITHUB_APP_PRIVATE_KEY or the GITHUB_APP_PRIVATE_KEY_PATH file).
    val githubAppPrivateKeyPem: String,
    // gitTransport selects the git clone/push transport: "https" (default — uses githubToken) or
    // "ssh" (local dev — ssh-agent/keys). SSH only covers the git transport; the GitHub REST API
    // (open/label PR, read CI) still needs a token, so an ssh run without a token warns at startup.
    val gitTransport: String,
    // gitSshKey is an explicit private-key path for gitTransport=ssh (GIT_SSH_KEY); empty falls
    // back to ssh-agent then the default identity files.
    val gitSshKey: String,
    // Notifications
    val notifyProvider: NotifyProvider,
    val slackWebhookUrl: String,
    val teamsWebhookUrl: String,
    // Server
    val port: String,
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
        require(gitTransport == "https" || gitTransport == "ssh") {
            "invalid GIT_TRANSPORT \"$gitTransport\" (want https|ssh)"
        }
        require(maxIterations >= 1) { "MAX_ITERATIONS must be >= 1, got $maxIterations" }
        val portNum = port.toIntOrNull()
            ?: throw IllegalArgumentException("PORT must be numeric, got \"$port\"")
        require(portNum in 1..65535) { "PORT must be in 1..65535, got $portNum" }
        // In App mode an installation can see every repo it is installed on, so an empty REPOS would
        // silently act on every installed repo. Require an explicit allowlist (Decision §4).
        require(!appMode() || repos.isNotEmpty()) {
            "REPOS must be set in GitHub App mode (empty REPOS = all repos is rejected to avoid acting on every installed repo)"
        }
    }

    /**
     * Whether GitHub App authentication is configured (the production auth path). False means PAT
     * mode (the local-dev fallback). The App ID is the discriminant — a zero value means App mode
     * is off, which is why a zero/negative App ID is rejected at load time.
     */
    fun appMode(): Boolean = githubAppId != 0L

    /**
     * Renders the config with every credential masked, so a debug/startup log that prints it never
     * leaks a secret. The data class's synthesized [toString] would otherwise dump the App private
     * key, PAT, webhook secret, internal token, and webhook URLs verbatim. Keep the secret set below
     * in sync when adding a sensitive field. (Mirrors Go's redacting `String()`, Python's
     * `repr=False`, and JS's `toJSON`.)
     */
    override fun toString(): String =
        "Config(" +
            "llmProvider=$llmProvider, ollamaHost=$ollamaHost, ollamaModel=$ollamaModel, " +
            "geminiModel=$geminiModel, ollamaCodeModel=$ollamaCodeModel, geminiCodeModel=$geminiCodeModel, " +
            "repos=$repos, githubToken=${redactSecret(githubToken)}, githubAppId=$githubAppId, " +
            "githubAppInstallationId=$githubAppInstallationId, " +
            "githubAppPrivateKeyPem=${redactSecret(githubAppPrivateKeyPem)}, gitTransport=$gitTransport, " +
            "gitSshKey=$gitSshKey, notifyProvider=$notifyProvider, " +
            "slackWebhookUrl=${redactSecret(slackWebhookUrl)}, teamsWebhookUrl=${redactSecret(teamsWebhookUrl)}, " +
            "port=$port, maxIterations=$maxIterations, ciTimeout=$ciTimeout, " +
            "githubWebhookSecret=${redactSecret(githubWebhookSecret)}, agentPrLabel=$agentPrLabel, " +
            "sessionBackend=$sessionBackend, sqliteDsn=$sqliteDsn, firestoreProject=$firestoreProject, " +
            "firestoreCollection=$firestoreCollection, internalToken=${redactSecret(internalToken)})"

    companion object {
        /** A function that resolves an environment key to its value, or null if unset. */
        fun interface Lookup {
            operator fun invoke(key: String): String?
        }

        /** Load reads configuration from the process environment, applying defaults. */
        fun load(): Config {
            val c = loadFrom { key -> System.getenv(key) }
            // When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's gh CLI
            // login so a local run authenticates to GitHub without a hand-set token. Skipped in App
            // mode: the App provider mints its own tokens, so shelling out to gh would be a useless
            // subprocess that could also hydrate a PAT the deployment never asked for.
            return if (!c.appMode() && c.githubToken.isEmpty()) c.copy(githubToken = ghCliToken()) else c
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

            // Resolve GitHub App credentials (production auth path). Absent App vars leave the zero
            // value — PAT mode; partial/misconfigured App vars are a startup error, never a silent
            // fallback to PAT (Decision §4).
            val app = resolveGitHubApp(get)

            val c = Config(
                llmProvider = llmProvider,
                ollamaHost = getOr(get, "OLLAMA_HOST", "http://localhost:11434"),
                ollamaModel = ollamaModel,
                geminiModel = geminiModel,
                ollamaCodeModel = ollamaCodeModel,
                geminiCodeModel = geminiCodeModel,
                repos = splitList(getOr(get, "REPOS", "")),
                githubToken = getOr(get, "GITHUB_TOKEN", getOr(get, "GH_TOKEN", "")),
                githubAppId = app.appId,
                githubAppInstallationId = app.installationId,
                githubAppPrivateKeyPem = app.privateKeyPem,
                gitTransport = getOr(get, "GIT_TRANSPORT", "https"),
                gitSshKey = getOr(get, "GIT_SSH_KEY", ""),
                notifyProvider = notifyProvider,
                slackWebhookUrl = getOr(get, "SLACK_WEBHOOK_URL", ""),
                teamsWebhookUrl = getOr(get, "TEAMS_WEBHOOK_URL", ""),
                port = getOr(get, "PORT", "8080"),
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

// Masks a secret for [Config.toString]: an unset value stays visibly empty (debugging a missing
// credential is common), a set value collapses to a fixed marker so its bytes never reach a log.
private fun redactSecret(s: String): String = if (s.isEmpty()) "\"\"" else "***"

// Trims so trailing whitespace/newlines on a value from the real environment
// (e.g. a CI secret with a trailing newline) do not leak into the setting.
private fun getOr(get: Config.Companion.Lookup, key: String, def: String): String {
    val v = get(key)?.trim()
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

/** The resolved GitHub App credentials. The zero value (appId == 0) means PAT mode. */
private data class GitHubApp(
    val appId: Long = 0L,
    val installationId: Long = 0L,
    val privateKeyPem: String = "",
)

/**
 * Reads the GITHUB_APP_* vars and decides the auth mode. With none set, returns the zero value (PAT
 * mode). With any set, App mode is intended and every requirement is enforced — App ID, a pinned
 * installation id, and exactly one private-key source — so a partial configuration is a startup
 * error, not a silent fallback to PAT (mode-selection rule, spec §"Config / env" + Decision §4).
 */
private fun resolveGitHubApp(get: Config.Companion.Lookup): GitHubApp {
    val appIdStr = getOr(get, "GITHUB_APP_ID", "")
    val installIdStr = getOr(get, "GITHUB_APP_INSTALLATION_ID", "")
    val keyLiteral = getOr(get, "GITHUB_APP_PRIVATE_KEY", "")
    val keyPath = getOr(get, "GITHUB_APP_PRIVATE_KEY_PATH", "")

    if (appIdStr.isEmpty() && installIdStr.isEmpty() && keyLiteral.isEmpty() && keyPath.isEmpty()) {
        return GitHubApp() // PAT mode — no App vars present.
    }
    // Any App var present signals intent to use App mode; require the full set.
    require(appIdStr.isNotEmpty()) { "GITHUB_APP_* set but GITHUB_APP_ID is missing (App mode requires GITHUB_APP_ID)" }
    require(installIdStr.isNotEmpty()) { "App mode requires GITHUB_APP_INSTALLATION_ID (single pinned installation)" }
    require(!(keyLiteral.isNotEmpty() && keyPath.isNotEmpty())) {
        "set exactly one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH, not both"
    }
    require(keyLiteral.isNotEmpty() || keyPath.isNotEmpty()) {
        "App mode requires one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH"
    }

    val appId = positiveId(appIdStr, "GITHUB_APP_ID")
    val installId = positiveId(installIdStr, "GITHUB_APP_INSTALLATION_ID")

    val raw = if (keyPath.isNotEmpty()) {
        try {
            File(keyPath).readText()
        } catch (e: IOException) {
            throw IllegalArgumentException("read GITHUB_APP_PRIVATE_KEY_PATH \"$keyPath\": ${e.message}", e)
        }
    } else {
        keyLiteral
    }
    val pem = normalizePrivateKeyPem(raw)
    // Validate the key parses as RSA now, so a bad key fails at startup with a clear message rather
    // than cryptically at the first token exchange. auth re-parses the same PEM for signing.
    parseRsaPrivateKey(pem)
    return GitHubApp(appId = appId, installationId = installId, privateKeyPem = pem)
}

/** Parses a strictly-positive id, rejecting non-numeric, zero, and negative values. */
private fun positiveId(raw: String, name: String): Long {
    val v = raw.toLongOrNull() ?: throw IllegalArgumentException("$name must be numeric, got \"$raw\"")
    require(v > 0) { "$name must be > 0, got $v" }
    return v
}

/**
 * Makes the App private key robust to how it is delivered (Decision §4): CI secret stores often
 * flatten newlines to the literal characters `\n`, so when the value looks like PEM and contains
 * escaped `\n` sequences, restore them — even if a real trailing newline is also present.
 */
private fun normalizePrivateKeyPem(raw: String): String =
    if (raw.contains("-----BEGIN") && raw.contains("\\n")) raw.replace("\\n", "\n") else raw

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
