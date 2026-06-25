package com.automation.agent.agent.fixflow

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.agents.ParallelAgent
import com.google.adk.kt.events.Event
import com.automation.agent.agent.setup.driveCollectState
import com.automation.agent.agent.setup.newRunner
import com.automation.agent.agent.setup.safeName
import com.automation.agent.agent.setup.textEvent
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow

private const val EDIT_PREFIX = "edit:" // state key per file: edit:<workPath> -> new content
private const val PATH_PREFIX = "path:" // state key per file: path:<workPath> -> target edit path

/**
 * Produces the edit for one file's work: the target path (which may differ from the source path —
 * e.g. a test file) and the new content. Return a zero [FileEdit] (empty path or content) to skip.
 */
fun interface EditFunc {
    suspend operator fun invoke(work: FileWork): FileEdit
}

/**
 * Fans out one analyzer agent per [FileWork] (ADK parallel agents, each writing distinct state keys
 * so they never collide), calls [fn] for each, and returns the collected non-empty edits sorted by
 * path.
 */
suspend fun parallelAnalyze(work: List<FileWork>, fn: EditFunc): List<FileEdit> {
    require(work.isNotEmpty()) { "analyze: no files to work on" }
    val sorted = work.sortedBy { it.path }

    val seen = mutableMapOf<String, Int>()
    val analyzers = sorted.map { AnalyzerAgent(uniqueAnalyzerName(seen, it.path), it, fn) }
    val parallel = ParallelAgent(name = "analyze_all", description = "Per-file analysis in parallel", subAgents = analyzers)
    val state = driveCollectState(newRunner("fix-analyze", parallel), "system", "analyze", "Produce the edits.")

    val edits =
        sorted.mapNotNull { w ->
            val content = state[EDIT_PREFIX + w.path] as? String
            val path = state[PATH_PREFIX + w.path] as? String
            if (!content.isNullOrBlank() && !path.isNullOrEmpty()) FileEdit(path = path, content = content) else null
        }
    require(edits.isNotEmpty()) { "analyze produced no edits" }
    return edits
}

/**
 * Derive a unique sub-agent name from a path. safeName maps every non-alphanumeric char to
 * `_`, so distinct paths (e.g. `a/b.kt` and `a-b.kt`) can collapse to the same name;
 * ParallelAgent needs unique sub-agent names, so a collision gets a numeric suffix —
 * otherwise one analyzer silently shadows another and that file's edits are dropped.
 */
private fun uniqueAnalyzerName(seen: MutableMap<String, Int>, path: String): String {
    val base = "analyze_${safeName(path)}"
    val n = (seen[base] ?: 0) + 1
    seen[base] = n
    return if (n > 1) "${base}_$n" else base
}

private class AnalyzerAgent(name: String, private val work: FileWork, private val fn: EditFunc) :
    BaseAgent(name = name, description = "Analyzes ${work.path}") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        val edit = fn(work)
        if (edit.path.isEmpty() || edit.content.isBlank()) {
            emit(textEvent(name, "skipped ${work.path}"))
        } else {
            emit(textEvent(name, "edited ${edit.path}", mapOf(EDIT_PREFIX + work.path to edit.content, PATH_PREFIX + work.path to edit.path)))
        }
    }
}
