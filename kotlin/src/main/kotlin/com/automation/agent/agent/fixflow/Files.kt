package com.automation.agent.agent.fixflow

import java.io.File
import java.io.IOException
import java.nio.file.Files
import java.nio.file.LinkOption
import java.nio.file.Path
import java.nio.file.Paths

/**
 * Resolves a repo-relative path against the checkout root, REJECTING (not clamping) absolute paths
 * and any path that escapes the root via ".." or a symlink. Both reads and writes route through it,
 * so LLM-controlled paths cannot touch host files.
 */
internal fun safeJoin(root: String, rel: String): String {
    require(!File(rel).isAbsolute) { "absolute path \"$rel\" not allowed" }
    val rootPath = Paths.get(root).normalize()
    val full = rootPath.resolve(rel).normalize()
    require(full == rootPath || full.startsWith(rootPath)) { "path \"$rel\" escapes the repo" }
    // Symlink containment: a symlinked directory inside the (attacker-influenced) checkout could
    // redirect an in-bounds path outside the root, so re-check the real location. toRealPath fails
    // on a not-yet-created target, so resolve the deepest existing ancestor; both sides resolved.
    val rootReal = rootPath.toRealPath()
    val fullReal = resolveExisting(full)
    require(fullReal != null && (fullReal == rootReal || fullReal.startsWith(rootReal))) {
        "path \"$rel\" escapes the repo via a symlink"
    }
    return full.toString()
}

/**
 * Returns [path] with its longest existing ancestor symlink-resolved and any not-yet-created
 * remainder appended lexically, or null if a path component exists but cannot be resolved
 * (a dangling or looping symlink), which could redirect a write outside the root.
 */
private fun resolveExisting(path: Path): Path? {
    var current: Path? = path
    val rest = ArrayDeque<Path>()
    while (current != null) {
        try {
            var resolved = current.toRealPath()
            for (segment in rest) resolved = resolved.resolve(segment)
            return resolved
        } catch (_: IOException) {
            // toRealPath failed. If the entry itself exists (NOFOLLOW), it's a dangling/
            // looping symlink — reject. Otherwise it's a genuinely missing component.
            if (Files.exists(current, LinkOption.NOFOLLOW_LINKS)) return null
            val name = current.fileName ?: return path // reached root with nothing resolved
            rest.addFirst(name)
            current = current.parent
        }
    }
    return path
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
