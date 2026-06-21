package io.github.jkjamies.automationagent.agent.fixflow

import java.io.File
import java.nio.file.Paths

/**
 * Resolves a repo-relative path against the checkout root, REJECTING (not clamping) absolute paths
 * and any path that escapes the root via "..". Both reads and writes route through it, so
 * LLM-controlled paths cannot touch host files. Resolution is lexical (no filesystem/symlink
 * access).
 */
internal fun safeJoin(root: String, rel: String): String {
    require(!File(rel).isAbsolute) { "absolute path \"$rel\" not allowed" }
    val rootPath = Paths.get(root).normalize()
    val full = rootPath.resolve(rel).normalize()
    require(full == rootPath || full.startsWith(rootPath)) { "path \"$rel\" escapes the repo" }
    return full.toString()
}

/** Reads a repo-relative file from the checkout (path-safe). */
internal fun readFile(root: String, rel: String): String = File(safeJoin(root, rel)).readText()

/**
 * Lists a checkout directory (path-safe), suffixing subdirectories with "/" and hiding the .git
 * directory. Sorted for determinism.
 */
internal fun listDirEntries(root: String, rel: String): List<String> {
    val full = safeJoin(root, rel)
    val entries = File(full).listFiles() ?: throw java.io.IOException("not a directory: $rel")
    return entries
        .filter { it.name != ".git" }
        .map { if (it.isDirectory) it.name + "/" else it.name }
        .sorted()
}
