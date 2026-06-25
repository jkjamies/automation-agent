package com.automation.agent.agent.lintfixer

import com.automation.agent.agent.fixflow.AnalyzeInput
import com.automation.agent.agent.fixflow.FileEdit
import com.automation.agent.agent.fixflow.FileWork
import com.automation.agent.agent.fixflow.parallelAnalyze
import com.automation.agent.agent.fixflow.readFile
import com.automation.agent.agent.fixflow.stripFences
import com.automation.agent.agent.setup.generateText

private val log: System.Logger = System.getLogger("automation-agent.lintfixer")

/**
 * Rewrites each affected source file to fix its lint problems, one parallel agent per file, reading
 * the current source from the checkout. Feedback (from a retry) is the previous attempt's CI
 * failure. An unreadable file is skipped.
 */
suspend fun analyze(input: AnalyzeInput): List<FileEdit> =
    parallelAnalyze(input.work) { w ->
        val src = runCatching { readFile(input.repoDir, w.path) }.getOrNull()
        if (src == null) {
            log.log(System.Logger.Level.WARNING, "lint analyze: skipping unreadable file ${w.path}")
            FileEdit("", "") // unreadable file -> skip
        } else {
            val model = requireNotNull(input.coder()) { "analyze: an LLM is required" }
            val out = generateText(model, prompts.get("analyze"), buildFilePrompt(w, src, input.feedback))
            FileEdit(path = w.path, content = stripFences(out))
        }
    }

internal fun buildFilePrompt(w: FileWork, content: String, ciFeedback: String): String =
    buildString {
        append("File: ${w.path}\n\nLint problems to fix:\n")
        w.items.forEach { append("- $it\n") }
        if (ciFeedback.isNotEmpty()) {
            append("\nThe previous attempt failed CI with:\n")
            append(ciFeedback)
            append("\n")
        }
        append("\nCurrent file content:\n```\n")
        append(content)
        append("\n```\n\nOutput ONLY the complete corrected content of this file — no explanation, no markdown fences.")
    }
