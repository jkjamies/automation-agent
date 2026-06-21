package io.github.jkjamies.automationagent.agent.fixflow

import com.google.adk.kt.tools.BaseTool
import com.google.adk.kt.tools.ToolContext
import com.google.adk.kt.types.FunctionDeclaration
import com.google.adk.kt.types.Schema
import com.google.adk.kt.types.Type

/**
 * Read-only tools (read_file, list_dir) rooted at the checkout, so a tool-using agent can examine
 * the real repository — its standards docs, existing tests, and layout — and ground decisions in
 * what the repo actually does. Tool failures self-wrap as `{"error": …}` rather than aborting the
 * run.
 */
internal fun repoTools(root: String): List<BaseTool> = listOf(ReadFileTool(root), ListDirTool(root))

private class ReadFileTool(private val root: String) :
    BaseTool(
        name = "read_file",
        description = "Read a repository file by its repo-relative path (e.g. \"src/main.kt\" or \"AGENTS.md\").",
    ) {
    override fun declaration(): FunctionDeclaration =
        FunctionDeclaration(
            name = name,
            description = description,
            parameters = Schema(type = Type.OBJECT, properties = mapOf("path" to Schema(type = Type.STRING, description = "repo-relative path")), required = listOf("path")),
        )

    override suspend fun run(context: ToolContext, args: Map<String, Any>): Any =
        try {
            mapOf("content" to readFile(root, args["path"] as? String ?: ""))
        } catch (e: Exception) {
            mapOf("error" to (e.message ?: e.toString()))
        }
}

private class ListDirTool(private val root: String) :
    BaseTool(
        name = "list_dir",
        description = "List the files and subdirectories of a repository directory by its repo-relative path. Use \".\" for the repository root.",
    ) {
    override fun declaration(): FunctionDeclaration =
        FunctionDeclaration(
            name = name,
            description = description,
            parameters = Schema(type = Type.OBJECT, properties = mapOf("path" to Schema(type = Type.STRING, description = "repo-relative path")), required = listOf("path")),
        )

    override suspend fun run(context: ToolContext, args: Map<String, Any>): Any =
        try {
            mapOf("entries" to listDirEntries(root, args["path"] as? String ?: "."))
        } catch (e: Exception) {
            mapOf("error" to (e.message ?: e.toString()))
        }
}
