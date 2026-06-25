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
import com.automation.agent.config.Config
import com.automation.agent.githubapi.Client
import com.automation.agent.githubapi.PrInput
import com.automation.agent.notify.Notifier
import com.automation.agent.notify.newNotifier
import com.automation.agent.webhook.SweepFunc
import com.automation.agent.webhook.webhookServer
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.job
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withTimeoutOrNull
import java.lang.System.Logger.Level
import java.time.Duration
import kotlin.system.exitProcess

private val log: System.Logger = System.getLogger("automation-agent")

/**
 * Caps how many webhook/cron dispatches run concurrently. A permit is acquired in the ingest path
 * BEFORE a dispatch coroutine is launched (admission backpressure: a burst blocks the ingest caller
 * rather than piling up unbounded coroutines), and released when the dispatch finishes. Matches the
 * Go reference's 32-permit dispatchSem (and Python/JS MAX_CONCURRENT_DISPATCH=32).
 */
private const val MAX_CONCURRENT_DISPATCH = 32

/** On SIGTERM: stop accepting requests within this grace, then this hard timeout, then drain. */
private const val SERVER_GRACE_MS = 5_000L
private const val SERVER_TIMEOUT_MS = 15_000L

/** How long to wait for in-flight dispatches to finish before abandoning them at shutdown. */
private const val DRAIN_TIMEOUT_MS = 20_000L

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

    val llm = buildLLM(cfg)
    val codeLlm = buildCodeLLM(cfg)

    val client = Client(token = cfg.githubToken)
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

    // Fix engines are event-driven and work without a notifier — they just won't post results.
    val fixDeps =
        Deps(
            gh = gh, llm = llm, codeLlm = codeLlm, notifier = notifier, token = cfg.githubToken,
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

    // Background scope for async dispatch from cron fires and webhook deliveries. In-flight
    // dispatches are bounded by a semaphore (see ingest below), not the dispatcher's parallelism —
    // the dispatch work hops onto Dispatchers.IO anyway, so a parallelism cap is not backpressure.
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // Bounds in-flight dispatches. The permit is acquired in the ingest path (both the webhook and
    // the scheduler/cron deliveries flow through this single boundary) BEFORE the coroutine is
    // launched, so a burst applies backpressure to the ingest caller instead of spawning unbounded
    // coroutines; it is released when the dispatch finishes. Mirrors Go's dispatchSem.
    val dispatchSem = Semaphore(MAX_CONCURRENT_DISPATCH)

    val dispatcher =
        buildRootDispatcher(
            RootDeps(
                summaryDaily = summaryDaily,
                lintKickoff = payloadHandler { lintEngine.kickoff(it) },
                coverageKickoff = payloadHandler { coverageEngine.kickoff(it) },
                ciResume = ciResumeHandler(engines),
                log = log,
            ),
        )

    if (cfg.githubWebhookSecret.isEmpty()) {
        log.log(
            Level.WARNING,
            "GITHUB_WEBHOOK_SECRET is unset — webhook signatures are NOT verified; " +
                "the /webhooks/github route accepts unauthenticated requests (dev only)",
        )
    }
    // Webhooks enqueue asynchronously and return fast.
    val server =
        webhookServer(
            port = cfg.port.toInt(),
            ingest = { envelope ->
                // When every permit is held, acquire() suspends the ingest caller here — the intended
                // backpressure. Surface it (like Python) so sustained saturation is observable rather
                // than a silently delayed webhook ACK.
                if (dispatchSem.availablePermits == 0) {
                    log.log(
                        Level.WARNING,
                        "dispatch concurrency saturated ($MAX_CONCURRENT_DISPATCH in flight); " +
                            "webhook ingest is applying backpressure until a slot frees",
                    )
                }
                // Acquire BEFORE launching so a burst blocks here instead of piling up coroutines;
                // the launched coroutine releases the permit when the dispatch finishes.
                dispatchSem.acquire()
                scope.launch {
                    try {
                        dispatcher.dispatch(envelope)
                    } catch (e: CancellationException) {
                        throw e
                    } catch (e: Exception) {
                        log.log(Level.WARNING, "webhook dispatch failed kind=${envelope.kind}", e)
                    } finally {
                        dispatchSem.release()
                    }
                }
            },
            secret = cfg.githubWebhookSecret,
            internalToken = cfg.internalToken,
            sweep = sweep,
        )

    // Graceful shutdown: stop firing crons, stop accepting requests, then drain in-flight
    // dispatches before exiting. Parked CI-wait runs still live only in memory and are abandoned on
    // restart (the documented in-memory trade); this only drains work already running.
    Runtime.getRuntime().addShutdownHook(
        Thread {
            log.log(Level.INFO, "shutting down: draining in-flight dispatches")
            server.stop(gracePeriodMillis = SERVER_GRACE_MS, timeoutMillis = SERVER_TIMEOUT_MS)
            runBlocking {
                withTimeoutOrNull(DRAIN_TIMEOUT_MS) { scope.coroutineContext.job.children.toList().joinAll() }
            }
            scope.cancel()
            // Release a durable park store's backing connection (a no-op for the memory backend).
            // close() is a plain blocking call, so no coroutine bridge is needed here.
            runCatching { parkStore.close() }
        },
    )

    log.log(
        Level.INFO,
        "automation-agent listening port=${cfg.port} llmProvider=${cfg.llmProvider} repos=${cfg.repos.size} notify=${cfg.notifyProvider} summaryEnabled=${summaryDaily != null}",
    )
    server.start(wait = true)
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
