package com.automation.agent.agent.covfixer

import com.google.adk.kt.models.Model
import com.automation.agent.agent.fixflow.FileWork
import com.automation.agent.agent.fixflow.extractJsonArray
import com.automation.agent.agent.setup.generateText
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

private val triageJson = Json { ignoreUnknownKeys = true }

/** Uses the LLM to normalize an arbitrary coverage report into the source files with meaningful uncovered logic. */
suspend fun triage(llm: Model?, report: String): List<FileWork> {
    val model = requireNotNull(llm) { "triage: an LLM is required" }
    val out = generateText(model, prompts.get("triage"), report)
    val work = parseTriage(out)
    require(work.isNotEmpty()) { "triage: no meaningful uncovered files found in report" }
    return work
}

@Serializable
private data class TriageFile(@SerialName("path") val path: String = "", @SerialName("uncovered") val uncovered: List<String> = emptyList())

internal fun parseTriage(out: String): List<FileWork> {
    val js = extractJsonArray(out)
    require(js.isNotEmpty()) { "no JSON array in model output" }
    val files =
        try {
            triageJson.decodeFromString<List<TriageFile>>(js)
        } catch (e: Exception) {
            throw IllegalArgumentException("decode triage JSON: ${e.message}")
        }
    return files.filter { it.path.isNotBlank() }.map { FileWork(path = it.path, items = it.uncovered) }
}
