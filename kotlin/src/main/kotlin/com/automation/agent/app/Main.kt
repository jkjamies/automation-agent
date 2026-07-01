/*
 * The automation-agent service entrypoint. It wires configuration, tooling, agents, and the
 * webhook server together, then runs until interrupted. Composition only — logic lives in
 * the feature packages.
 */
package com.automation.agent.app

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.models.Model
import com.automation.agent.agent.covfixer.newEngine as newCoverageEngine
import com.automation.agent.agent.fixflow.Deps
import com.automation.agent.agent.fixflow.Engine
import com.automation.agent.agent.fixflow.GitHub
import com.automation.agent.agent.lintfixer.newEngine as newLintEngine
import com.automation.agent.agent.root.Handler
import com.automation.agent.agent.root.RootDeps
import com.automation.agent.agent.root.buildRootDispatcher
import com.automation.agent.agent.setup.buildCodeLLM
import com.automation.agent.agent.setup.buildLLM
import com.automation.agent.agent.setup.newParkStore
import com.automation.agent.agent.setup.newSessionService
import com.automation.agent.agent.summary.CommitLister
import com.automation.agent.agent.summary.SummaryDeps
import com.automation.agent.agent.summary.buildSummaryAgent
import com.automation.agent.auth.StaticProvider
import com.automation.agent.auth.TokenProvider
import com.automation.agent.auth.newAppProvider
import com.automation.agent.config.Config
import com.automation.agent.githubapi.Client
import com.automation.agent.githubapi.PrInput
import com.automation.agent.notify.Notifier
import com.automation.agent.notify.newNotifier
import com.automation.agent.obs.Config as ObsConfig
import com.automation.agent.obs.init as initTracing
import com.automation.agent.obs.installObsTracing
import com.automation.agent.obs.newLogHandler
import com.automation.agent.config.TasksBackend
import com.automation.agent.tasks.DEFAULT_MAX_CONCURRENT
import com.automation.agent.tasks.DispatchFunc
import com.automation.agent.tasks.InProcess
import com.automation.agent.tasks.Transport
import com.automation.agent.tasks.newCloudTasks
import com.automation.agent.webhook.SweepFunc
import com.automation.agent.webhook.webhookServer
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.runBlocking
import java.lang.System.Logger.Level
import java.time.Duration
import kotlin.system.exitProcess

private val log: System.Logger = System.getLogger("automation-agent")

/** On SIGTERM: stop accepting requests within this grace, then this hard timeout, then drain. */
private const val SERVER_GRACE_MS = 5_000L
private const val SERVER_TIMEOUT_MS = 15_000L

fun main() {
    try {
        run()
    } catch (e: Exception) {
        log.log(Level.ERROR, "fatal", e)
        exitProcess(1)
    }
}

private fun run() {
    val cfg = Config.load()

    // Turn on distributed tracing (off by default). init runs once, right after config load and
    // before any agent runs, so the agent framework's native spans resolve our provider. shutdownTracing
    // force-flushes buffered spans at exit (the scale-to-zero span-loss guard). rlog wraps the injected
    // logger so records emitted under a span carry trace_id / span_id.
    val shutdownTracing = initTracing(
        ObsConfig(
            exporter = cfg.otelTracesExporter,
            serviceName = cfg.otelServiceName,
            otlpEndpoint = cfg.otelExporterOtlpEndpoint,
            otlpHeaders = cfg.otelExporterOtlpHeaders,
            sampler = cfg.otelTracesSampler,
        ),
    )
    val rlog = newLogHandler(log)

    val llm = buildLLM(cfg)
    val codeLlm = buildCodeLLM(cfg)

    // The auth seam: App mode (production) mints/caches installation tokens; otherwise a static PAT
    // (or empty/anonymous). The REST client and the git layer share this one provider.
    val provider = buildTokenProvider(cfg)
    val client = Client(tokenSource = { provider.token("") })
    val commitLister = CommitLister { owner, repo, since -> client.listCommitsSince(owner, repo, since) }
    val gh = githubAdapter(client)
    val notifier = buildNotifier(cfg)

    val summaryDaily = buildSummary(cfg, llm, commitLister, notifier, Duration.ofHours(24), "Daily commit digest")
    // /internal/cron/daily is the only daily-digest trigger, and it 404s when INTERNAL_TOKEN
    // is unset. Warn rather than fail silently so a built-but-unreachable digest is visible.
    if (summaryDaily != null && cfg.internalToken.isEmpty()) {
        log.log(
            Level.WARNING,
            "daily summary built but INTERNAL_TOKEN is unset; /internal/cron/daily is disabled (404), so the digest cannot be triggered",
        )
    }

    // One session service + park store, shared by both fix engines. memory (the default) keeps
    // today's behavior; the durable backends persist parked runs across restarts.
    val sessionService = newSessionService(cfg)
    val parkStore = newParkStore(cfg)

    // SSH only covers the git transport; the GitHub REST API (open/label PR, read CI) still needs a
    // token. Warn rather than fail when ssh is selected without one — read-only/dry-run flows may not
    // hit the API, but any PR operation will fail.
    if (cfg.gitTransport == "ssh" && cfg.githubToken.isEmpty() && !cfg.appMode()) {
        log.log(
            Level.WARNING,
            "GIT_TRANSPORT=ssh but no GitHub token found (GITHUB_TOKEN/GH_TOKEN/`gh auth token`); " +
                "git clone+push will use ssh, but PR operations against the REST API will fail — " +
                "run `gh auth login` or set a token",
        )
    }

    // Fix engines are event-driven and work without a notifier — they just won't post results.
    val fixDeps =
        Deps(
            gh = gh, llm = llm, codeLlm = codeLlm, notifier = notifier,
            provider = { repo -> provider.token(repo) },
            gitTransport = cfg.gitTransport, sshKey = cfg.gitSshKey,
            maxIter = cfg.maxIterations, ciTimeout = cfg.ciTimeout, repos = cfg.repos,
            prLabel = cfg.agentPrLabel,
            sessionService = sessionService, parkStore = parkStore,
        )
    val lintEngine = newLintEngine(fixDeps)
    val coverageEngine = newCoverageEngine(fixDeps)
    val engines = listOf(lintEngine, coverageEngine)

    // The durable timeout catch-all behind POST /internal/sweep: resolve every engine's parked runs
    // whose CI never reported (Cloud Scheduler drives it). One engine's failure must not stop the
    // others, so collect-and-continue across ALL engines, then surface every failure (the first with
    // the rest attached as suppressed — the JVM equivalent of Go's errors.Join / JS's AggregateError)
    // so the handler 500s and Cloud Scheduler retries. Cancellation is rethrown, never collected.
    val sweep =
        SweepFunc {
            val errors = mutableListOf<Exception>()
            for (engine in engines) {
                try {
                    engine.sweepTimeouts()
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    log.log(Level.WARNING, "sweep failed for an engine", e)
                    errors += e
                }
            }
            errors.firstOrNull()?.let { primary ->
                errors.drop(1).forEach { primary.addSuppressed(it) }
                throw primary
            }
        }

    val dispatcher =
        buildRootDispatcher(
            RootDeps(
                summaryDaily = summaryDaily,
                lintKickoff = payloadHandler { lintEngine.kickoff(it) },
                coverageKickoff = payloadHandler { coverageEngine.kickoff(it) },
                ciResume = ciResumeHandler(engines),
                log = rlog,
            ),
        )

    // The execution transport runs the dispatch: in-process (default) on a bounded coroutine pool
    // drained on SIGTERM, or — in production — via Cloud Tasks, which delivers each envelope to
    // /internal/dispatch so the compute runs in-request (CPU stays allocated) with durable retry.
    // See specs/20260626-workflow-execution-transport.md.
    val transport = buildTransport(cfg, rlog) { envelope -> dispatcher.dispatch(envelope) }

    if (cfg.githubWebhookSecret.isEmpty()) {
        log.log(
            Level.WARNING,
            "GITHUB_WEBHOOK_SECRET is unset — webhook signatures are NOT verified; " +
                "the /webhooks/github route accepts unauthenticated requests (dev only)",
        )
    }
    // Webhooks enqueue onto the transport and return fast; /internal/dispatch (the Cloud Tasks
    // worker) runs the same dispatcher in-request.
    val server =
        webhookServer(
            port = cfg.port.toInt(),
            ingest = { envelope -> transport.enqueue(envelope) },
            secret = cfg.githubWebhookSecret,
            internalToken = cfg.internalToken,
            sweep = sweep,
            dispatch = { envelope -> dispatcher.dispatch(envelope) },
            log = rlog,
            // A server span per request (the trace root on ingress, continued from the task's
            // traceparent on /internal/dispatch), force-flushed before the response returns. A no-op
            // when tracing is disabled.
            configure = { installObsTracing() },
        )

    // Graceful shutdown: stop accepting requests, then drain in-flight dispatches before exiting.
    // Parked CI-wait runs still live only in memory and are abandoned on restart (the documented
    // in-memory trade); this only drains work already running.
    Runtime.getRuntime().addShutdownHook(
        Thread {
            log.log(Level.INFO, "shutting down: draining in-flight dispatches")
            server.stop(gracePeriodMillis = SERVER_GRACE_MS, timeoutMillis = SERVER_TIMEOUT_MS)
            // Close the transport after the server stops accepting: the in-process backend drains
            // in-flight dispatches (bounded), the Cloud Tasks backend closes its client. Done before
            // the park store closes so any draining dispatch still has its stores.
            runBlocking { transport.close() }
            // Release a durable park store's backing connection (a no-op for the memory backend).
            runCatching { parkStore.close() }
            // Flush and release the tracer provider last, so any span from a draining dispatch or the
            // shutdown path itself is exported before the process exits. Isolated so a store-close
            // failure above cannot skip it, and its own failure cannot mask the rest.
            runCatching { shutdownTracing() }
        },
    )

    log.log(
        Level.INFO,
        "automation-agent listening port=${cfg.port} llmProvider=${cfg.llmProvider} repos=${cfg.repos.size} notify=${cfg.notifyProvider} summaryEnabled=${summaryDaily != null}",
    )
    server.start(wait = true)
}

/**
 * Builds the GitHub auth provider: App mode (production — a validated App id, installation id, and
 * exactly one private-key source) mints/caches short-lived installation tokens for the pinned
 * installation; otherwise a [StaticProvider] over the resolved PAT (or empty/anonymous). One provider
 * backs both the REST client and the git layer.
 */
private fun buildTokenProvider(cfg: Config): TokenProvider =
    if (cfg.appMode()) {
        newAppProvider(cfg.githubAppId, cfg.githubAppInstallationId, cfg.githubAppPrivateKeyPem)
    } else {
        StaticProvider(cfg.githubToken)
    }

/**
 * Selects the webhook execution transport: Cloud Tasks in production (durable, in-request,
 * rate-limited by the queue) or the in-process coroutine pool for local dev (the default). The
 * in-process backend runs [dispatch] directly; the Cloud Tasks backend instead delivers each
 * envelope to /internal/dispatch, which the webhook wires to the same dispatcher.
 * See specs/20260626-workflow-execution-transport.md.
 */
private fun buildTransport(cfg: Config, log: System.Logger, dispatch: DispatchFunc): Transport {
    if (cfg.tasksBackend == TasksBackend.CLOUDTASKS) {
        log.log(
            Level.INFO,
            "execution transport: cloud tasks project=${cfg.tasksProject} location=${cfg.tasksLocation} " +
                "queue=${cfg.tasksQueue} dispatchUrl=${cfg.dispatchUrl}",
        )
        return newCloudTasks(
            cfg.tasksProject, cfg.tasksLocation, cfg.tasksQueue, cfg.dispatchUrl, cfg.internalToken, cfg.tasksDispatchDeadline,
        )
    }
    log.log(Level.INFO, "execution transport: in-process (local/default)")
    return InProcess(dispatch, log, DEFAULT_MAX_CONCURRENT)
}

/** Returns a Notifier, or null (with a warning) if not configured. */
private fun buildNotifier(cfg: Config): Notifier? =
    try {
        newNotifier(cfg.notifyProvider.value, cfg.slackWebhookUrl, cfg.teamsWebhookUrl)
    } catch (e: Exception) {
        log.log(Level.WARNING, "notifier not configured; summary disabled and fixers won't post: ${e.message}")
        null
    }

/**
 * Returns a summary workflow agent for the given look-back [window] and digest [title], or null if
 * it can't be fully configured (no repos / notifier). The daily Cloud Scheduler trigger fires it.
 */
private fun buildSummary(cfg: Config, llm: Model, gh: CommitLister, notifier: Notifier?, window: Duration, title: String): BaseAgent? {
    if (cfg.repos.isEmpty()) {
        log.log(Level.WARNING, "no REPOS configured; summary workflow disabled")
        return null
    }
    if (notifier == null) return null // buildNotifier already warned
    return try {
        buildSummaryAgent(SummaryDeps(llm = llm, gh = gh, notifier = notifier, repos = cfg.repos, window = window, title = title))
    } catch (e: Exception) {
        log.log(Level.WARNING, "summary workflow disabled: ${e.message}")
        null
    }
}

/** Adapts the GitHub client to the fix engines' narrow [GitHub] interface. */
private fun githubAdapter(client: Client): GitHub =
    object : GitHub {
        override suspend fun findOpenPrByBranch(owner: String, repo: String, branch: String) = client.findOpenPrByBranch(owner, repo, branch)
        override suspend fun createPr(owner: String, repo: String, input: PrInput) = client.createPr(owner, repo, input)
        override suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>) = client.addLabels(owner, repo, number, labels)
        override suspend fun compare(owner: String, repo: String, base: String, head: String) = client.compare(owner, repo, base, head)
    }

/** Adapts a raw-payload kickoff/resume function to a dispatcher [Handler]. */
private fun payloadHandler(f: suspend (ByteArray) -> Unit): Handler = Handler { envelope -> f(envelope.payload) }

/**
 * Hands a check_run event to every engine; each no-ops unless its check name matches. Collects
 * every engine's failure (the first with the rest attached as suppressed, mirroring Go's
 * errors.Join) and rethrows cancellation rather than collecting it.
 */
private fun ciResumeHandler(engines: List<Engine>): Handler =
    Handler { envelope ->
        val errors = mutableListOf<Exception>()
        for (engine in engines) {
            try {
                engine.resume(envelope.payload)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                errors += e
            }
        }
        errors.firstOrNull()?.let { primary ->
            errors.drop(1).forEach { primary.addSuppressed(it) }
            throw primary
        }
    }
