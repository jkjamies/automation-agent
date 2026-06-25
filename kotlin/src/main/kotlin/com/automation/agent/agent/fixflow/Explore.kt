package com.automation.agent.agent.fixflow

import com.google.adk.kt.agents.Instruction
import com.google.adk.kt.agents.LlmAgent
import com.google.adk.kt.models.Model
import com.automation.agent.agent.setup.driveText
import com.automation.agent.agent.setup.newRunner

/**
 * Runs a single tool-using agent that navigates the checkout itself (via read_file/list_dir) to
 * examine the real repository — standards docs, existing tests, layout — and returns its final text
 * answer. The model decides what to read; no code pre-selects files. Workflows use this to ground a
 * plan (e.g. where tests belong) in the repo's actual conventions rather than a hardcoded rule.
 */
suspend fun explore(llm: Model, repoDir: String, instruction: String, input: String): String {
    val agent =
        LlmAgent(
            name = "explorer",
            description = "Examines the repository to ground a plan in its real conventions.",
            model = llm,
            instruction = Instruction(instruction),
            tools = repoTools(repoDir),
        )
    return driveText(newRunner("fix-explore", agent), "system", "explore", input)
}
