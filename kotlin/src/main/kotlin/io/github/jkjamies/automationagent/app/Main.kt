/*
 * The automation-agent service entrypoint. It wires configuration, tooling, agents, the scheduler,
 * and the webhook server together, then runs until interrupted. Composition only — logic lives in
 * the feature packages.
 */
package io.github.jkjamies.automationagent.app

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.models.Model
import io.github.jkjamies.automationagent.agent.covfixer.newEngine as newCoverageEngine
import io.github.jkjamies.automationagent.agent.fixflow.Deps
import io.github.jkjamies.automationagent.agent.fixflow.Engine
import io.github.jkjamies.automationagent.agent.fixflow.GitHub
import io.github.jkjamies.automationagent.agent.lintfixer.newEngine as newLintEngine
import io.github.jkjamies.automationagent.agent.root.Handler
import io.github.jkjamies.automationagent.agent.root.RootDeps
import io.github.jkjamies.automationagent.agent.root.buildRootDispatcher
import io.github.jkjamies.automationagent.agent.setup.buildCodeLLM
import io.github.jkjamies.automationagent.agent.setup.buildLLM
import io.github.jkjamies.automationagent.agent.setup.newParkStore
import io.github.jkjamies.automationagent.agent.setup.newSessionService
import io.github.jkjamies.automationagent.agent.summary.CommitLister
import io.github.jkjamies.automationagent.agent.summary.SummaryDeps
import io.github.jkjamies.automationagent.agent.summary.buildSummaryAgent
import io.github.jkjamies.automationagent.config.Config
import io.github.jkjamies.automationagent.githubapi.Client
import io.github.jkjamies.automationagent.githubapi.PrInput
import io.github.jkjamies.automationagent.ingest.Kind
import io.github.jkjamies.automationagent.notify.Notifier
import io.github.jkjamies.automationagent.notify.newNotifier
import io.github.jkjamies.automationagent.scheduler.Scheduler
import io.github.jkjamies.automationagent.webhook.SweepFunc
import io.github.jkjamies.automationagent.webhook.webhookServer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.job
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull
import java.lang.System.Logger.Level
import java.time.Duration
import kotlin.system.exitProcess

private val log: System.Logger = System.getLogger("automation-agent")

/** Caps how many webhook/cron dispatches run concurrently, so a burst can't spawn unbounded work. */
private const val MAX_CONCURRENT_DISPATCH = 16

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
    val summaryWeekly = buildSummary(cfg, llm, commitLister, notifier, Duration.ofDays(7), "Weekly commit digest")

    // One session service + park store, shared by both fix engines. memory (the default) keeps
    // today's behavior; the durable backends persist parked runs across restarts.
    val sessionService = newSessionService(cfg)
    val parkStore = newParkStore(cfg)

    // Fix engines are event-driven and work without a notifier — they just won't post results.
    val fixDeps =
        Deps(
            gh = gh, llm = llm, codeLlm = codeLlm, notifier = notifier, token = cfg.githubToken,
            maxIter = cfg.maxIterations, ciTimeout = cfg.ciTimeout, repos = cfg.repos,
            sessionService = sessionService, parkStore = parkStore,
        )
    val lintEngine = newLintEngine(fixDeps)
    val coverageEngine = newCoverageEngine(fixDeps)
    val engines = listOf(lintEngine, coverageEngine)

    // The durable timeout catch-all behind POST /internal/sweep: resolve every engine's parked runs
    // whose CI never reported (Cloud Scheduler drives it). One engine's failure must not stop the
    // others, so collect-and-continue, then surface so the handler 500s and Cloud Scheduler retries.
    val sweep =
        SweepFunc {
            var first: Throwable? = null
            for (engine in engines) {
                runCatching { engine.sweepTimeouts() }.onFailure {
                    log.log(Level.WARNING, "sweep failed for an engine", it)
                    if (first == null) first = it
                }
            }
            first?.let { throw it }
        }

    // Background scope for async dispatch from cron fires and webhook deliveries. Parallelism is
    // bounded so a burst of deliveries can't spawn unbounded coroutines.
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default.limitedParallelism(MAX_CONCURRENT_DISPATCH))

    val dispatcher =
        buildRootDispatcher(
            RootDeps(
                summaryDaily = summaryDaily,
                summaryWeekly = summaryWeekly,
                lintKickoff = payloadHandler { lintEngine.kickoff(it) },
                coverageKickoff = payloadHandler { coverageEngine.kickoff(it) },
                ciResume = ciResumeHandler(engines),
                log = log,
            ),
        )

    // Scheduler: cron fires dispatch on the background scope.
    val scheduler =
        Scheduler({ envelope ->
            scope.launch {
                runCatching { dispatcher.dispatch(envelope) }
                    .onFailure { log.log(Level.WARNING, "scheduled dispatch failed kind=${envelope.kind}", it) }
            }
        })
    scheduler.add(cfg.cronDaily, Kind.CRON_DAILY)
    scheduler.add(cfg.cronWeekly, Kind.CRON_WEEKLY)
    scheduler.start()

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
                scope.launch {
                    runCatching { dispatcher.dispatch(envelope) }
                        .onFailure { log.log(Level.WARNING, "webhook dispatch failed kind=${envelope.kind}", it) }
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
            scheduler.stop()
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
 * it can't be fully configured (no repos / notifier). Daily and weekly each get their own agent so
 * the weekly cron produces a 7-day, "Weekly"-titled digest rather than a daily one.
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
        override suspend fun findAgentPrs(owner: String, repo: String, label: String) = client.findAgentPrs(owner, repo, label)
        override suspend fun createPr(owner: String, repo: String, input: PrInput) = client.createPr(owner, repo, input)
        override suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>) = client.addLabels(owner, repo, number, labels)
        override suspend fun compare(owner: String, repo: String, base: String, head: String) = client.compare(owner, repo, base, head)
    }

/** Adapts a raw-payload kickoff/resume function to a dispatcher [Handler]. */
private fun payloadHandler(f: suspend (ByteArray) -> Unit): Handler = Handler { envelope -> f(envelope.payload) }

/** Hands a check_run event to every engine; each no-ops unless its check name matches. */
private fun ciResumeHandler(engines: List<Engine>): Handler =
    Handler { envelope ->
        var first: Throwable? = null
        for (engine in engines) {
            runCatching { engine.resume(envelope.payload) }.onFailure { if (first == null) first = it }
        }
        first?.let { throw it }
    }
