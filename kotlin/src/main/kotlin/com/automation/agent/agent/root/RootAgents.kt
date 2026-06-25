package com.automation.agent.agent.root

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.runners.Runner
import com.automation.agent.agent.setup.drive
import com.automation.agent.agent.setup.newRunner
import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind

/**
 * Wires the dispatcher. Each handler is optional. [ciResume] handles [Kind.CI] for every fix
 * workflow (lint, coverage) — each engine no-ops unless its check matches. [summaryDaily] runs the
 * daily commit digest fired by the daily Cloud Scheduler trigger.
 */
data class RootDeps(
    val summaryDaily: BaseAgent? = null,
    val lintKickoff: Handler? = null,
    val coverageKickoff: Handler? = null,
    val ciResume: Handler? = null,
    val log: System.Logger? = null,
)

/**
 * Builds the dispatcher and registers the available workflows: [Kind.CRON_DAILY] → summary;
 * [Kind.LINT] → lint-fixer; [Kind.COVERAGE] → coverage-fixer; [Kind.CI] → resume (all fix engines).
 */
fun buildRootDispatcher(deps: RootDeps): Dispatcher {
    val dispatcher = Dispatcher(deps.log)

    deps.summaryDaily?.let { dispatcher.register(Kind.CRON_DAILY, summaryHandler(newRunner("automation-agent", it))) }
    deps.lintKickoff?.let { dispatcher.register(Kind.LINT, it) }
    deps.coverageKickoff?.let { dispatcher.register(Kind.COVERAGE, it) }
    deps.ciResume?.let { dispatcher.register(Kind.CI, it) }
    return dispatcher
}

/** Drives a summary workflow runner for a cron envelope, using a fresh session per fire. */
private fun summaryHandler(runner: Runner): Handler =
    Handler { envelope ->
        val sessionId = "summary-${envelope.receivedAt.toEpochMilli()}"
        drive(runner, "system", sessionId, "Run the commit digest.")
    }
