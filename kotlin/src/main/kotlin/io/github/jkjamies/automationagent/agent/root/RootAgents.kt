package io.github.jkjamies.automationagent.agent.root

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.runners.Runner
import io.github.jkjamies.automationagent.agent.setup.drive
import io.github.jkjamies.automationagent.agent.setup.newRunner
import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind

/**
 * Wires the dispatcher. Each handler is optional. [ciResume] handles [Kind.CI] for every fix
 * workflow (lint, coverage) — each engine no-ops unless its check matches.
 */
data class RootDeps(
    val summaryAgent: BaseAgent? = null,
    val lintKickoff: Handler? = null,
    val coverageKickoff: Handler? = null,
    val ciResume: Handler? = null,
    val log: System.Logger? = null,
)

/**
 * Builds the dispatcher and registers the available workflows: cron kinds → summary;
 * [Kind.LINT] → lint-fixer; [Kind.COVERAGE] → coverage-fixer; [Kind.CI] → resume (all fix engines).
 */
fun buildRootDispatcher(deps: RootDeps): Dispatcher {
    val dispatcher = Dispatcher(deps.log)

    deps.summaryAgent?.let { agent ->
        val handler = summaryHandler(newRunner("automation-agent", agent))
        dispatcher.register(Kind.CRON_DAILY, handler)
        dispatcher.register(Kind.CRON_WEEKLY, handler)
    }
    deps.lintKickoff?.let { dispatcher.register(Kind.LINT, it) }
    deps.coverageKickoff?.let { dispatcher.register(Kind.COVERAGE, it) }
    deps.ciResume?.let { dispatcher.register(Kind.CI, it) }
    return dispatcher
}

/** Drives the summary workflow runner for a cron envelope, using a fresh session per fire. */
private fun summaryHandler(runner: Runner): Handler =
    Handler { envelope ->
        val sessionId = "summary-${envelope.receivedAt.toEpochMilli()}"
        drive(runner, "system", sessionId, "Run the daily commit digest.")
    }
