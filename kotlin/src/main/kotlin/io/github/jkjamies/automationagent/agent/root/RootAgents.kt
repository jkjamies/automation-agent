package io.github.jkjamies.automationagent.agent.root

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.runners.Runner
import io.github.jkjamies.automationagent.agent.setup.drive
import io.github.jkjamies.automationagent.agent.setup.newRunner
import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind

/**
 * Wires the dispatcher. Each handler is optional. [ciResume] handles [Kind.CI] for every fix
 * workflow (lint, coverage) — each engine no-ops unless its check matches. [summaryDaily] and
 * [summaryWeekly] are distinct agents (different look-back window and digest title), so the Monday
 * cron produces a genuine weekly digest rather than a daily-titled 24h one.
 */
data class RootDeps(
    val summaryDaily: BaseAgent? = null,
    val summaryWeekly: BaseAgent? = null,
    val lintKickoff: Handler? = null,
    val coverageKickoff: Handler? = null,
    val ciResume: Handler? = null,
    val log: System.Logger? = null,
)

/**
 * Builds the dispatcher and registers the available workflows: [Kind.CRON_DAILY]/[Kind.CRON_WEEKLY]
 * → their respective summaries; [Kind.LINT] → lint-fixer; [Kind.COVERAGE] → coverage-fixer;
 * [Kind.CI] → resume (all fix engines).
 */
fun buildRootDispatcher(deps: RootDeps): Dispatcher {
    val dispatcher = Dispatcher(deps.log)

    deps.summaryDaily?.let { dispatcher.register(Kind.CRON_DAILY, summaryHandler(newRunner("automation-agent", it))) }
    deps.summaryWeekly?.let { dispatcher.register(Kind.CRON_WEEKLY, summaryHandler(newRunner("automation-agent", it))) }
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
