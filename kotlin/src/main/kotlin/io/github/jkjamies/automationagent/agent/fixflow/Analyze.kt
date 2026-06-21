package io.github.jkjamies.automationagent.agent.fixflow

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.agents.ParallelAgent
import com.google.adk.kt.events.Event
import io.github.jkjamies.automationagent.agent.setup.driveCollectState
import io.github.jkjamies.automationagent.agent.setup.newRunner
import io.github.jkjamies.automationagent.agent.setup.textEvent
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

    val analyzers = sorted.map { AnalyzerAgent(it, fn) }
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

private class AnalyzerAgent(private val work: FileWork, private val fn: EditFunc) :
    BaseAgent(name = "analyze_${safeName(work.path)}", description = "Analyzes ${work.path}") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        val edit = fn(work)
        if (edit.path.isEmpty() || edit.content.isBlank()) {
            emit(textEvent(name, "skipped ${work.path}"))
        } else {
            emit(textEvent(name, "edited ${edit.path}", mapOf(EDIT_PREFIX + work.path to edit.content, PATH_PREFIX + work.path to edit.path)))
        }
    }
}

/** Replaces every non-ASCII-alphanumeric character with `_`, for a safe agent name. */
internal fun safeName(s: String): String =
    s.map { c -> if (c in 'a'..'z' || c in 'A'..'Z' || c in '0'..'9') c else '_' }.joinToString("")
