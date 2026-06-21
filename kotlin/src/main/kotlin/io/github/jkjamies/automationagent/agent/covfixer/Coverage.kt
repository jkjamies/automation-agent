/*
 * Package covfixer is the test-coverage configuration of the fixflow engine: it triages an agnostic
 * coverage report into source files with meaningful uncovered logic, then generates language-aware
 * tests for them. Its prompts are entirely separate from the lint-fixer's; only the deterministic
 * loop is shared (fixflow).
 */
package io.github.jkjamies.automationagent.agent.covfixer

import io.github.jkjamies.automationagent.agent.fixflow.Deps
import io.github.jkjamies.automationagent.agent.fixflow.Engine
import io.github.jkjamies.automationagent.agent.fixflow.Spec
import io.github.jkjamies.automationagent.agent.setup.Prompts

internal val prompts = Prompts.forAgent("covfixer")

/** Builds the coverage-fixer engine. */
fun newEngine(deps: Deps): Engine =
    Engine(
        Spec(
            name = "coverage",
            branch = "automation-agent/test-coverage",
            label = "automation-agent-coverage",
            checkName = "agent-coverage-verify",
            commitMessage = "automation-agent: add test coverage",
            prTitle = "automation-agent: add test coverage",
            successTitle = "Coverage fix succeeded ✅",
            reviewTitle = "Coverage fix needs human review ⚠️",
            triage = ::triage,
            analyze = ::analyze,
        ),
        deps,
    )
