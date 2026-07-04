/*
 * Package config loads the automation-agent runtime configuration from the
 * environment. It is the single source of truth for settings; no other package
 * should read the environment directly. See ../okf/standards/architecture-design.md §12.
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

/*
 * Trace-exporter values for [Config.otelTracesExporter]. The app speaks exactly these four sinks and
 * never names a vendor; obs mirrors them. See obs and okf/standards/observability.md.
 */
const val OTEL_EXPORTER_NONE = "none"
const val OTEL_EXPORTER_CONSOLE = "console"
const val OTEL_EXPORTER_OTLP = "otlp"
const val OTEL_EXPORTER_GCP = "gcp"

// Reviewer intake defaults (pilot-tunable). Held byte-for-byte identical across every port so a
// review is sized and steered the same way everywhere.
private const val DEFAULT_REVIEW_MAX_FILES = 50
private const val DEFAULT_REVIEW_MAX_DIFF_BYTES = 256 * 1024 // 256 KiB
private const val DEFAULT_REVIEW_MIN_CONFIDENCE = 0.6
private const val DEFAULT_REVIEW_STANDARDS_MAX_BYTES = 256 * 1024 // 256 KiB

// The paths dropped before sizing/review: lockfiles, generated code, vendored trees, minified
// bundles, snapshots, and binaries. A pattern with no '/' matches the basename; one with '/' matches
// the full path ("**" crosses separators).
private const val DEFAULT_REVIEW_EXCLUDE_GLOBS =
    "go.sum,go.work.sum,package-lock.json,yarn.lock,pnpm-lock.yaml," +
        "npm-shrinkwrap.json,Cargo.lock,poetry.lock,Pipfile.lock,Gemfile.lock,composer.lock," +
        "gradle.lockfile,*.min.js,*.min.css,*.map,*.snap,*.pb.go,*_pb2.py,*.gen.go,*_generated.go," +
        "vendor/**,node_modules/**,third_party/**,dist/**,build/**,__snapshots__/**," +
        "*.png,*.jpg,*.jpeg,*.gif,*.webp,*.ico,*.pdf,*.woff,*.woff2,*.ttf,*.eot," +
        "*.zip,*.gz,*.tar,*.jar,*.bin,*.so,*.dylib,*.dll,*.exe"

// The convention-doc paths discovered in the reviewed repo — format-agnostic across the common
// AI-assistant and project conventions. A pattern with no '/' matches the basename; one with '/'
// matches the full path.
private const val DEFAULT_REVIEW_STANDARDS_GLOBS =
    "AGENTS.md,**/AGENTS.md,CLAUDE.md,**/CLAUDE.md,GEMINI.md,**/GEMINI.md," +
        ".cursor/rules/**,.cursorrules,.claude/**,.github/copilot-instructions.md," +
        ".windsurfrules,.windsurf/rules/**,.agents/standards/**,CONTRIBUTING.md," +
        ".editorconfig,.golangci.yml,.golangci.yaml"

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

/**
 * TasksBackend selects the webhook execution transport: how an enqueued envelope reaches the
 * dispatcher.
 */
enum class TasksBackend(val value: String) {
    /** Runs each dispatch in a bounded in-process coroutine pool (the pre-transport behavior). */
    INPROCESS("inprocess"),

    /**
     * Enqueues each envelope as a Cloud Tasks HTTP-target task pointed at /internal/dispatch, which
     * executes it in-request (CPU stays allocated) with durable retry — the production backend.
     */
    CLOUDTASKS("cloudtasks"),
    ;

    override fun toString(): String = value

    companion object {
        fun from(s: String): TasksBackend? = entries.firstOrNull { it.value == s }
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
    // startup error, never a silent fallback. See [appMode].
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
    // Reviewer (PR code-review agent). reviewEnabled (REVIEW_ENABLED) is the kill switch: the engine
    // no-ops unless it is set, so the feature is dark by default and the rollback posture is a single
    // flag. The other vars tune intake, standards-aware review, and the debounce/coalesce window; all
    // are held byte-for-byte identical across every port.
    val reviewEnabled: Boolean,
    // reviewSkipDrafts skips draft PRs unless the triggering action is ready_for_review
    // (REVIEW_SKIP_DRAFTS, default true).
    val reviewSkipDrafts: Boolean,
    // reviewExcludeGlobs drops generated/vendored/lockfile/minified/binary paths before sizing and
    // review (REVIEW_EXCLUDE_GLOBS). Defaults to the built-in exclude list.
    val reviewExcludeGlobs: List<String>,
    // reviewMaxFiles / reviewMaxDiffBytes are the two-dimensional size-gate caps (REVIEW_MAX_FILES,
    // REVIEW_MAX_DIFF_BYTES): a PR over either cap (measured on the filtered diff) is denied rather
    // than degraded. A non-positive value disables that dimension.
    val reviewMaxFiles: Int,
    val reviewMaxDiffBytes: Int,
    // reviewStandards toggles standards-aware review (REVIEW_STANDARDS, default true): discover the
    // reviewed repo's own convention docs, distill them, and steer the lenses off them.
    // reviewStandardsGlobs are the discovery globs (REVIEW_STANDARDS_GLOBS); reviewStandardsMaxBytes
    // caps the total doc bytes fed to the distiller (REVIEW_STANDARDS_MAX_BYTES). reviewUncitedMode
    // (REVIEW_UNCITED_MODE, drop|nitpick, default nitpick) is how an uncited conformance finding is
    // handled.
    val reviewStandards: Boolean,
    val reviewStandardsGlobs: List<String>,
    val reviewStandardsMaxBytes: Int,
    val reviewUncitedMode: String,
    // reviewMinConfidence drops findings below this confidence before scoring (REVIEW_MIN_CONFIDENCE,
    // the phase-1 verify gate). A non-positive value keeps everything.
    val reviewMinConfidence: Double,
    // reviewDebounce coalesces rapid pushes (REVIEW_DEBOUNCE): a synchronize review is enqueued under
    // a per-PR dedup name with this delay, so a burst of pushes collapses to one review of the latest
    // SHA. opened/reopened/ready_for_review enqueue immediately.
    val reviewDebounce: Duration,
    // Sessions: where the durable suspend/resume session + park record live.
    val sessionBackend: SessionBackend,
    // sqliteDsn is the database path for SESSION_BACKEND=sqlite (ignored otherwise).
    val sqliteDsn: String,
    // firestoreProject is the GCP project for SESSION_BACKEND=firestore; empty detects it from
    // ADC / GOOGLE_CLOUD_PROJECT. firestoreCollection is the collection-name prefix.
    val firestoreProject: String,
    val firestoreCollection: String,
    // internalToken is the Bearer token guarding the /internal/* endpoints (Cloud Scheduler
    // cron + sweep + dispatch). Empty disables those routes (they 404).
    val internalToken: String,
    // Execution transport (webhook -> dispatcher). tasksBackend selects in-process (default) or
    // Cloud Tasks. The Cloud Tasks settings locate the queue and the worker endpoint; they are
    // required (and validated) only in cloudtasks mode.
    val tasksBackend: TasksBackend,
    // tasksProject is the GCP project owning the queue (TASKS_PROJECT); empty falls back to
    // GOOGLE_CLOUD_PROJECT. Required for cloudtasks.
    val tasksProject: String,
    // tasksLocation is the queue's region (TASKS_LOCATION, e.g. "us-central1"). Required for cloudtasks.
    val tasksLocation: String,
    // tasksQueue is the Cloud Tasks queue name (TASKS_QUEUE). Required for cloudtasks.
    val tasksQueue: String,
    // dispatchUrl is the full URL of the /internal/dispatch worker the queue POSTs to (DISPATCH_URL,
    // e.g. https://agent-xyz.run.app/internal/dispatch). Required for cloudtasks.
    val dispatchUrl: String,
    // tasksDispatchDeadline is how long Cloud Tasks waits for an /internal/dispatch attempt before
    // cancelling it and retrying (TASKS_DISPATCH_DEADLINE). Set explicitly because the HTTP-target
    // default is only 10m, far short of a multi-minute workflow. Cloud Tasks caps it at 30m, which
    // is therefore the default and the ceiling. Used only in cloudtasks mode.
    val tasksDispatchDeadline: Duration,
    // Observability (distributed tracing). Everything is additive and off by default: with
    // otelTracesExporter=none nothing is registered and the service is unchanged. The agent
    // framework emits the spans natively; obs only wires the provider. These are read here (the
    // single place that reads OTEL_*) and handed to obs as a typed struct. See obs and
    // okf/standards/observability.md.
    // otelTracesExporter selects the sink: none | console | otlp | gcp.
    val otelTracesExporter: String,
    // otelTracesExporterSet records whether OTEL_TRACES_EXPORTER was explicitly provided, so the
    // playground can default to console for local dev without overriding an explicit choice.
    val otelTracesExporterSet: Boolean,
    // otelServiceName is the resource service.name on every span (OTEL_SERVICE_NAME).
    val otelServiceName: String,
    // otelExporterOtlpEndpoint / otelExporterOtlpHeaders configure the otlp exporter (standard
    // OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_HEADERS). The endpoint is required for otlp;
    // the headers commonly carry a vendor API key and are masked in the config log view.
    val otelExporterOtlpEndpoint: String,
    val otelExporterOtlpHeaders: String,
    // otelTracesSampler is a standard OTEL_TRACES_SAMPLER value; the default parentbased_always_on
    // records every locally-started trace and honors an upstream decision.
    val otelTracesSampler: String,
    // otelCaptureMessageContent gates whether prompt/response bodies are captured as span content
    // (sensitive; off by default). The agent framework reads the standard
    // OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT natively — surfacing it here keeps it under
    // config's single-reader ownership and documents the switch.
    val otelCaptureMessageContent: Boolean,
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
        validateTasks()
        validateOtel()
        validateReviewer()
    }

    /**
     * Checks the REVIEW_* settings that defaults alone cannot guarantee: the standards byte cap must
     * be positive, the confidence gate must be a probability, and the uncited mode must be one of the
     * two known values.
     */
    private fun validateReviewer() {
        require(reviewStandardsMaxBytes > 0) {
            "REVIEW_STANDARDS_MAX_BYTES: must be positive, got $reviewStandardsMaxBytes"
        }
        require(reviewMinConfidence in 0.0..1.0) {
            "REVIEW_MIN_CONFIDENCE: must be in [0,1], got $reviewMinConfidence"
        }
        require(reviewUncitedMode == "drop" || reviewUncitedMode == "nitpick") {
            "invalid REVIEW_UNCITED_MODE \"$reviewUncitedMode\" (want drop|nitpick)"
        }
    }

    /**
     * Checks the OTEL_* settings: the exporter must be one of the four known sinks, and the otlp
     * exporter needs an endpoint (it would otherwise silently export nowhere). The other sinks need
     * nothing extra.
     */
    private fun validateOtel() {
        when (otelTracesExporter) {
            OTEL_EXPORTER_NONE, OTEL_EXPORTER_CONSOLE, OTEL_EXPORTER_GCP -> Unit
            OTEL_EXPORTER_OTLP -> require(otelExporterOtlpEndpoint.isNotBlank()) {
                "OTEL_TRACES_EXPORTER=otlp requires OTEL_EXPORTER_OTLP_ENDPOINT"
            }
            else -> throw IllegalArgumentException(
                "invalid OTEL_TRACES_EXPORTER \"$otelTracesExporter\" (want none|console|otlp|gcp)",
            )
        }
    }

    /**
     * Validates the execution-transport settings. The in-process backend needs nothing. The Cloud
     * Tasks backend needs the queue coordinates and worker URL, plus the Bearer token the task
     * carries: without INTERNAL_TOKEN, /internal/dispatch is disabled (404) and every task would
     * fail permanently. Fail fast rather than silently never dispatching.
     */
    private fun validateTasks() {
        if (tasksBackend == TasksBackend.INPROCESS) return
        val missing = mutableListOf<String>()
        if (tasksProject.isEmpty()) missing += "TASKS_PROJECT (or GOOGLE_CLOUD_PROJECT)"
        if (tasksLocation.isEmpty()) missing += "TASKS_LOCATION"
        if (tasksQueue.isEmpty()) missing += "TASKS_QUEUE"
        // DISPATCH_URL must be an absolute https URL ending in /internal/dispatch: the task carries
        // INTERNAL_TOKEN as a Bearer header to it, so a plaintext http:// target would leak the
        // token in transit, and a base URL or stray path would pass the scheme check then 404 every
        // task at runtime. A suffix match (not equality) tolerates a gateway path prefix.
        if (!isSecureDispatchUrl(dispatchUrl)) {
            missing += "DISPATCH_URL (must be an absolute https:// URL ending in /internal/dispatch)"
        }
        if (internalToken.isEmpty()) missing += "INTERNAL_TOKEN (the Bearer the task carries to /internal/dispatch)"
        // Cloud Tasks clamps an HTTP-target dispatch deadline to 15s..30m; a value outside that range
        // is silently rejected at CreateTask, so reject it here instead.
        if (tasksDispatchDeadline < 15.seconds || tasksDispatchDeadline > 30.minutes) {
            missing += "TASKS_DISPATCH_DEADLINE (must be between 15s and 30m)"
        }
        // In Cloud Tasks mode the deployment is production-facing, so an unverified webhook surface
        // is a real exposure rather than a dev convenience — require the HMAC secret (it stays an
        // opt-in warning only for the local inprocess default).
        if (githubWebhookSecret.isEmpty()) {
            missing += "GITHUB_WEBHOOK_SECRET (webhook signatures must be verified in production)"
        }
        require(missing.isEmpty()) { "TASKS_BACKEND=cloudtasks requires ${missing.joinToString(", ")}" }
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
            "reviewEnabled=$reviewEnabled, reviewSkipDrafts=$reviewSkipDrafts, reviewExcludeGlobs=$reviewExcludeGlobs, " +
            "reviewMaxFiles=$reviewMaxFiles, reviewMaxDiffBytes=$reviewMaxDiffBytes, reviewStandards=$reviewStandards, " +
            "reviewStandardsGlobs=$reviewStandardsGlobs, reviewStandardsMaxBytes=$reviewStandardsMaxBytes, " +
            "reviewUncitedMode=$reviewUncitedMode, reviewMinConfidence=$reviewMinConfidence, reviewDebounce=$reviewDebounce, " +
            "sessionBackend=$sessionBackend, sqliteDsn=$sqliteDsn, firestoreProject=$firestoreProject, " +
            "firestoreCollection=$firestoreCollection, internalToken=${redactSecret(internalToken)}, " +
            "tasksBackend=$tasksBackend, tasksProject=$tasksProject, tasksLocation=$tasksLocation, " +
            "tasksQueue=$tasksQueue, dispatchUrl=$dispatchUrl, tasksDispatchDeadline=$tasksDispatchDeadline, " +
            "otelTracesExporter=$otelTracesExporter, otelServiceName=$otelServiceName, " +
            "otelExporterOtlpEndpoint=$otelExporterOtlpEndpoint, " +
            "otelExporterOtlpHeaders=${redactSecret(otelExporterOtlpHeaders)}, " +
            "otelTracesSampler=$otelTracesSampler, otelCaptureMessageContent=$otelCaptureMessageContent)"

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

            val tasksBackendRaw = getOr(get, "TASKS_BACKEND", TasksBackend.INPROCESS.value)
            val tasksBackend = TasksBackend.from(tasksBackendRaw)
                ?: throw IllegalArgumentException("invalid TASKS_BACKEND \"$tasksBackendRaw\" (want inprocess|cloudtasks)")

            val dispatchDeadlineRaw = getOr(get, "TASKS_DISPATCH_DEADLINE", "30m")
            val tasksDispatchDeadline = parseGoDuration(dispatchDeadlineRaw)
                ?: throw IllegalArgumentException("TASKS_DISPATCH_DEADLINE: invalid duration \"$dispatchDeadlineRaw\"")

            val reviewDebounceRaw = getOr(get, "REVIEW_DEBOUNCE", "30s")
            val reviewDebounce = parseGoDuration(reviewDebounceRaw)
                ?: throw IllegalArgumentException("REVIEW_DEBOUNCE: invalid duration \"$reviewDebounceRaw\"")

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
                reviewEnabled = getBool(get, "REVIEW_ENABLED", false),
                reviewSkipDrafts = getBool(get, "REVIEW_SKIP_DRAFTS", true),
                reviewExcludeGlobs = splitList(getOr(get, "REVIEW_EXCLUDE_GLOBS", DEFAULT_REVIEW_EXCLUDE_GLOBS)),
                reviewMaxFiles = getInt(get, "REVIEW_MAX_FILES", DEFAULT_REVIEW_MAX_FILES),
                reviewMaxDiffBytes = getInt(get, "REVIEW_MAX_DIFF_BYTES", DEFAULT_REVIEW_MAX_DIFF_BYTES),
                reviewStandards = getBool(get, "REVIEW_STANDARDS", true),
                reviewStandardsGlobs = splitList(getOr(get, "REVIEW_STANDARDS_GLOBS", DEFAULT_REVIEW_STANDARDS_GLOBS)),
                reviewStandardsMaxBytes = getInt(get, "REVIEW_STANDARDS_MAX_BYTES", DEFAULT_REVIEW_STANDARDS_MAX_BYTES),
                reviewUncitedMode = getOr(get, "REVIEW_UNCITED_MODE", "nitpick"),
                reviewMinConfidence = getFloat(get, "REVIEW_MIN_CONFIDENCE", DEFAULT_REVIEW_MIN_CONFIDENCE),
                reviewDebounce = reviewDebounce,
                tasksBackend = tasksBackend,
                // TASKS_PROJECT falls back to GOOGLE_CLOUD_PROJECT (the ambient Cloud Run var).
                tasksProject = getOr(get, "TASKS_PROJECT", getOr(get, "GOOGLE_CLOUD_PROJECT", "")),
                tasksLocation = getOr(get, "TASKS_LOCATION", ""),
                tasksQueue = getOr(get, "TASKS_QUEUE", ""),
                dispatchUrl = getOr(get, "DISPATCH_URL", ""),
                tasksDispatchDeadline = tasksDispatchDeadline,
                otelTracesExporter = getOr(get, "OTEL_TRACES_EXPORTER", OTEL_EXPORTER_NONE),
                // Blank/unset means not explicitly chosen; an explicit "none" still counts as set.
                otelTracesExporterSet = !get("OTEL_TRACES_EXPORTER").isNullOrBlank(),
                otelServiceName = getOr(get, "OTEL_SERVICE_NAME", "automation-agent"),
                otelExporterOtlpEndpoint = getOr(get, "OTEL_EXPORTER_OTLP_ENDPOINT", ""),
                otelExporterOtlpHeaders = getOr(get, "OTEL_EXPORTER_OTLP_HEADERS", ""),
                otelTracesSampler = getOr(get, "OTEL_TRACES_SAMPLER", "parentbased_always_on"),
                otelCaptureMessageContent =
                    getBool(get, "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", false),
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

// Parses a boolean flag, tolerating the common spellings; a blank/unset or unrecognized value
// yields [def] so a typo never flips a sensitive default on.
private fun getBool(get: Config.Companion.Lookup, key: String, def: Boolean): Boolean {
    val v = get(key)?.trim()?.lowercase()
    if (v.isNullOrEmpty()) return def
    return when (v) {
        "1", "true", "t", "yes", "on" -> true
        "0", "false", "f", "no", "off" -> false
        else -> def
    }
}

// Parses an integer setting (REVIEW_MAX_FILES etc.); a blank/unset value yields [def], a non-integer
// value is a startup error.
private fun getInt(get: Config.Companion.Lookup, key: String, def: Int): Int {
    val v = get(key)?.trim()
    if (v.isNullOrEmpty()) return def
    return v.toIntOrNull() ?: throw IllegalArgumentException("$key: invalid integer \"$v\"")
}

// Parses a floating-point setting (REVIEW_MIN_CONFIDENCE); a blank/unset value yields [def], a
// non-numeric value is a startup error.
private fun getFloat(get: Config.Companion.Lookup, key: String, def: Double): Double {
    val v = get(key)?.trim()
    if (v.isNullOrEmpty()) return def
    return v.toDoubleOrNull() ?: throw IllegalArgumentException("$key: invalid number \"$v\"")
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

/** Whether [raw] is an absolute https URL whose path ends in /internal/dispatch. */
private fun isSecureDispatchUrl(raw: String): Boolean {
    if (raw.isEmpty()) return false
    val uri = try {
        java.net.URI(raw)
    } catch (_: java.net.URISyntaxException) {
        return false
    }
    return uri.scheme == "https" && !uri.host.isNullOrEmpty() && uri.path?.endsWith("/internal/dispatch") == true
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
