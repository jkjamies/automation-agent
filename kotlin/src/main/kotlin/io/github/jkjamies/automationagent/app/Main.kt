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
import io.github.jkjamies.automationagent.webhook.webhookServer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import java.lang.System.Logger.Level
import kotlin.system.exitProcess

private val log: System.Logger = System.getLogger("automation-agent")

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

    val summaryAgent = buildSummary(cfg, llm, commitLister, notifier)

    // Fix engines are event-driven and work without a notifier — they just won't post results.
    val fixDeps =
        Deps(
            gh = gh, llm = llm, codeLlm = codeLlm, notifier = notifier, token = cfg.githubToken,
            maxIter = cfg.maxIterations, ciTimeout = cfg.ciTimeout,
        )
    val lintEngine = newLintEngine(fixDeps)
    val coverageEngine = newCoverageEngine(fixDeps)
    val engines = listOf(lintEngine, coverageEngine)

    // Background scope for async dispatch from cron fires and webhook deliveries.
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    val dispatcher =
        buildRootDispatcher(
            RootDeps(
                summaryAgent = summaryAgent,
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
    Runtime.getRuntime().addShutdownHook(Thread { scheduler.stop() })

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
        )

    log.log(
        Level.INFO,
        "automation-agent listening port=${cfg.port} llmProvider=${cfg.llmProvider} repos=${cfg.repos.size} notify=${cfg.notifyProvider} summaryEnabled=${summaryAgent != null}",
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

/** Returns the summary workflow agent, or null if it can't be fully configured (no repos / notifier). */
private fun buildSummary(cfg: Config, llm: Model, gh: CommitLister, notifier: Notifier?): BaseAgent? {
    if (cfg.repos.isEmpty()) {
        log.log(Level.WARNING, "no REPOS configured; summary workflow disabled")
        return null
    }
    if (notifier == null) return null // buildNotifier already warned
    return try {
        buildSummaryAgent(SummaryDeps(llm = llm, gh = gh, notifier = notifier, repos = cfg.repos))
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
