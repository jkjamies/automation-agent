package com.automation.agent.agent.covfixer

import com.automation.agent.agent.fixflow.AnalyzeInput
import com.automation.agent.agent.fixflow.FileEdit
import com.automation.agent.agent.fixflow.FileWork
import com.automation.agent.agent.fixflow.explore
import com.automation.agent.agent.fixflow.extractJsonArray
import com.automation.agent.agent.fixflow.parallelAnalyze
import com.automation.agent.agent.fixflow.readFile
import com.automation.agent.agent.fixflow.stripFences
import com.automation.agent.agent.setup.generateText
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

private val planJson = Json { ignoreUnknownKeys = true }

private val log: System.Logger = System.getLogger("automation-agent.covfixer")

/**
 * Plans test placement by having a tool-using agent examine the checked-out repo's real conventions,
 * then generates a test per file in parallel from that plan.
 */
suspend fun analyze(input: AnalyzeInput): List<FileEdit> {
    val plan = explorePlan(input)
    return execute(input, plan)
}

/** The explorer's decision for one source file, grounded in the repo's actual existing tests. */
@Serializable
internal data class PlanEntry(
    @SerialName("source") val source: String = "",
    @SerialName("test_path") val testPath: String = "",
    @SerialName("framework") val framework: String = "",
    @SerialName("notes") val notes: String = "",
)

/**
 * Runs a tool-using agent that navigates the checkout itself (read_file / list_dir) to learn the
 * repo's real test conventions and returns a per-file plan keyed by source path.
 */
private suspend fun explorePlan(input: AnalyzeInput): Map<String, PlanEntry> {
    val model = requireNotNull(input.llm) { "explore: an LLM is required" }
    val out = explore(model, input.repoDir, prompts.get("explore"), buildExploreInput(input.work))
    val plan = parsePlan(out)
    require(plan.isNotEmpty()) { "explore: produced no test placements" }
    return plan
}

/** Writes each test from the plan + source, one parallel agent per file. */
private suspend fun execute(input: AnalyzeInput, plan: Map<String, PlanEntry>): List<FileEdit> =
    parallelAnalyze(input.work) { w ->
        val entry = plan[w.path]
        val src = if (entry == null || entry.testPath.isBlank()) null else runCatching { readFile(input.repoDir, w.path) }.getOrNull()
        if (entry == null || src == null) {
            val reason = if (entry == null) "no test placement from explorer" else "unreadable source"
            log.log(System.Logger.Level.WARNING, "coverage analyze: skipping ${w.path} ($reason)")
            FileEdit("", "") // explorer couldn't place it, or unreadable -> skip
        } else {
            val model = requireNotNull(input.coder()) { "execute: an LLM is required" }
            val out = generateText(model, prompts.get("analyze"), buildExecuteInput(w, src, entry, input.feedback))
            FileEdit(path = entry.testPath, content = stripFences(out))
        }
    }

internal fun parsePlan(out: String): Map<String, PlanEntry> {
    val js = extractJsonArray(out)
    require(js.isNotEmpty()) { "no JSON array in explorer output" }
    val entries =
        try {
            planJson.decodeFromString<List<PlanEntry>>(js)
        } catch (e: Exception) {
            throw IllegalArgumentException("decode plan JSON: ${e.message}")
        }
    return entries.filter { it.source.isNotBlank() }.associateBy { it.source }
}

internal fun buildExploreInput(work: List<FileWork>): String =
    buildString {
        append("Source files that need tests:\n")
        work.forEach { append("- ${it.path}\n") }
    }

internal fun buildExecuteInput(w: FileWork, src: String, p: PlanEntry, ciFeedback: String): String =
    buildString {
        append("Write the test file at: ${p.testPath}\nFramework / convention: ${p.framework}\n")
        if (p.notes.isNotBlank()) append("Notes: ${p.notes}\n")
        append("\nUncovered logic to cover:\n")
        w.items.forEach { append("- $it\n") }
        if (ciFeedback.isNotEmpty()) {
            append("\nThe previous attempt failed CI with:\n")
            append(ciFeedback)
            append("\n")
        }
        append("\nSource file (${w.path}):\n```\n")
        append(src)
        append("\n```\n")
    }
