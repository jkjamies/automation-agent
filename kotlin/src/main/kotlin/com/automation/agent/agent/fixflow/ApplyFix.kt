package com.automation.agent.agent.fixflow

import com.automation.agent.githubapi.Comparison
import com.automation.agent.githubapi.Pr
import com.automation.agent.githubapi.PrInput
import com.automation.agent.gitrepo.Author
import com.automation.agent.gitrepo.Repo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File
import java.nio.file.Files

/** The slice of `githubapi` the apply step needs (consumer-defined, fakeable). */
interface GitHub {
    suspend fun findOpenPrByBranch(owner: String, repo: String, branch: String): Pr?
    suspend fun createPr(owner: String, repo: String, input: PrInput): Pr
    suspend fun addLabels(owner: String, repo: String, number: Int, labels: List<String>)

    /** The base...head comparison for a terminal summary (what the agent changed on the PR). */
    suspend fun compare(owner: String, repo: String, base: String, head: String): Comparison
}

/** A whole-file write an analyze step produces (a rewritten source file, a generated test, …). */
data class FileEdit(val path: String, val content: String)

/** Parameterizes one apply. */
data class ApplyConfig(
    val owner: String,
    val repo: String,
    val cloneUrl: String,
    val token: String = "",
    /**
     * Explicit private-key path for an ssh [cloneUrl] (GIT_SSH_KEY); empty falls back to ssh-agent
     * then the default identity files. Ignored for an https [cloneUrl].
     */
    val sshKey: String = "",
    val base: String, // base branch the PR targets
    val branch: String, // agent working branch
    val newBranch: Boolean, // true on kickoff (create from base); false on retry (reuse remote branch)
    val label: String,
    val commitMessage: String,
    val prTitle: String,
    val prBody: String,
    val author: Author,
)

/** The outcome of one apply. */
data class ApplyResult(val pr: Pr, val headSha: String)

/**
 * Clones the repo into a fresh temp dir and checks out the agent branch — the single checkout the
 * explorer reads, the executor writes into, and the commit step pushes. [ApplyConfig.newBranch]
 * creates the branch from HEAD (kickoff); false checks out the existing remote branch (retry). The
 * caller must delete `repo.dir()` when done.
 */
suspend fun open(cfg: ApplyConfig): Repo {
    val dir = withContext(Dispatchers.IO) { Files.createTempDirectory("agentfix-").toFile() }
    val repo =
        try {
            Repo.clone(cfg.cloneUrl, dir.path, cfg.token, cfg.sshKey)
        } catch (e: Throwable) {
            withContext(Dispatchers.IO) { dir.deleteRecursively() }
            throw e
        }
    try {
        if (cfg.newBranch) repo.checkout(cfg.branch, create = true) else repo.checkoutRemote(cfg.branch)
    } catch (e: Throwable) {
        withContext(Dispatchers.IO) { dir.deleteRecursively() }
        throw e
    }
    return repo
}

/** Writes edits into the working tree, commits, pushes, and ensures a labeled PR exists. */
suspend fun commit(gh: GitHub, repo: Repo, cfg: ApplyConfig, edits: List<FileEdit>): ApplyResult {
    require(edits.isNotEmpty()) { "apply: no edits to apply" }
    writeEdits(repo, edits)
    val sha = repo.commitAll(cfg.commitMessage, cfg.author)
    repo.push()
    val pr = ensurePR(gh, cfg)
    return ApplyResult(pr = pr, headSha = sha)
}

/**
 * Opens a checkout and commits edits in one step (no analysis in between) — a convenience used in
 * tests; the engine interleaves analysis between [open] and [commit].
 */
suspend fun applyFix(gh: GitHub, cfg: ApplyConfig, edits: List<FileEdit>): ApplyResult {
    val repo = open(cfg)
    return try {
        commit(gh, repo, cfg, edits)
    } finally {
        // Release the JGit handles before deleting the checkout (see Engine.attemptOnce).
        repo.close()
        withContext(Dispatchers.IO) { File(repo.dir()).deleteRecursively() }
    }
}

private suspend fun writeEdits(repo: Repo, edits: List<FileEdit>) = withContext(Dispatchers.IO) {
    for (edit in edits) {
        // Reject LLM-controlled paths that escape the checkout (path traversal).
        val full =
            try {
                safeJoin(repo.dir(), edit.path)
            } catch (e: IllegalArgumentException) {
                throw IllegalArgumentException("reject edit \"${edit.path}\": ${e.message}")
            }
        File(full).parentFile?.mkdirs()
        File(full).writeText(edit.content)
    }
}

/**
 * Returns the existing open PR for the branch, or creates and labels one. Lookup is by head branch
 * (not the agent label, which is write-only and never read back).
 */
private suspend fun ensurePR(gh: GitHub, cfg: ApplyConfig): Pr {
    gh.findOpenPrByBranch(cfg.owner, cfg.repo, cfg.branch)?.let { return it }
    val pr = gh.createPr(cfg.owner, cfg.repo, PrInput(title = cfg.prTitle, head = cfg.branch, base = cfg.base, body = cfg.prBody))
    gh.addLabels(cfg.owner, cfg.repo, pr.number, listOf(cfg.label))
    return pr.copy(labels = pr.labels + cfg.label)
}
