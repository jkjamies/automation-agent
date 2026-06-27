/*
 * Package lintfixer is the lint-remediation configuration of the fixflow engine: it supplies a
 * triage step (normalize a linter report) and an analyze step (rewrite the affected source files),
 * plus its branch/label/check identity. The loop itself lives in agent.fixflow.
 */
package com.automation.agent.agent.lintfixer

import com.automation.agent.agent.fixflow.Deps
import com.automation.agent.agent.fixflow.Engine
import com.automation.agent.agent.fixflow.Spec
import com.automation.agent.agent.setup.Prompts

internal val prompts = Prompts.forAgent("lintfixer")

/** Builds the lint-fixer engine. */
fun newEngine(deps: Deps): Engine =
    Engine(
        Spec(
            name = "lint",
            branch = "automation-agent/lint-fix",
            checkName = "agent-lint-verify",
            commitMessage = "automation-agent: fix lint problems",
            prTitle = "automation-agent: fix lint problems",
            successTitle = "Lint fix succeeded ✅",
            reviewTitle = "Lint fix needs human review ⚠️",
            cleanTitle = "Lint already clean 👏",
            triage = ::triage,
            analyze = ::analyze,
        ),
        deps,
    )
