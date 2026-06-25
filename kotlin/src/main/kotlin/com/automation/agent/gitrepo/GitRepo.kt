/*
 * Package gitrepo wraps JGit for the working-tree operations the fixers need: clone, branch,
 * stage-all, commit, push. Pure-JVM (no git binary). Deterministic tooling — no agent
 * imports.
 */
package com.automation.agent.gitrepo

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.eclipse.jgit.api.CreateBranchCommand.SetupUpstreamMode
import org.eclipse.jgit.api.Git
import org.eclipse.jgit.lib.Constants
import org.eclipse.jgit.lib.PersonIdent
import org.eclipse.jgit.transport.CredentialsProvider
import org.eclipse.jgit.transport.UsernamePasswordCredentialsProvider
import java.io.File
import java.time.Instant
import java.time.ZoneId

/** Identifies the committer. */
data class Author(val name: String, val email: String)

/** Raised by [Repo.commitAll] when the working tree is clean. */
class NoChangesException : Exception("gitrepo: no changes to commit")

/**
 * A cloned working tree. The underlying JGit [Git] owns a [org.eclipse.jgit.lib.Repository] with
 * open file handles / pack locks, so [Repo] is [AutoCloseable]: a long-running service doing
 * repeated fix loops must close each clone (via `use {}` or a `finally`) to avoid an fd/lock leak.
 */
class Repo internal constructor(
    private val git: Git,
    private val dir: File,
    private val cred: CredentialsProvider?,
    private val now: () -> Instant,
) : AutoCloseable {
    /** The working-tree directory; callers write file edits under it. */
    fun dir(): String = dir.path

    /** Joins [rel] onto the working-tree directory. */
    fun path(rel: String): String = File(dir, rel).path

    /** Switches to [branch], creating it from the current HEAD when [create] is true. */
    suspend fun checkout(branch: String, create: Boolean) = withContext(Dispatchers.IO) {
        git.checkout().setName(branch).setCreateBranch(create).call()
        Unit
    }

    /**
     * Checks out an existing remote branch (origin/<branch>) as a local branch — used on
     * retry iterations to add a commit onto the previous fix rather than starting a new
     * branch from the base. Throws if origin/<branch> does not exist.
     */
    suspend fun checkoutRemote(branch: String) = withContext(Dispatchers.IO) {
        git.checkout()
            .setName(branch)
            .setCreateBranch(true)
            .setStartPoint("origin/$branch")
            .setUpstreamMode(SetupUpstreamMode.TRACK)
            .call()
        Unit
    }

    /**
     * Stages every change (including deletions) and commits, returning the new commit SHA.
     * Throws [NoChangesException] if the tree is clean.
     */
    suspend fun commitAll(msg: String, a: Author): String = withContext(Dispatchers.IO) {
        git.add().addFilepattern(".").call() // new + modified
        git.add().setUpdate(true).addFilepattern(".").call() // deletions of tracked files
        if (git.status().call().isClean) throw NoChangesException()
        val who = PersonIdent(a.name, a.email, now(), ZoneId.systemDefault())
        git.commit().setMessage(msg).setAuthor(who).setCommitter(who).call().name
    }

    /** Pushes the current branch to origin. An up-to-date push is not an error. */
    suspend fun push() = withContext(Dispatchers.IO) {
        val cmd = git.push()
        cred?.let { cmd.setCredentialsProvider(it) }
        cmd.add(git.repository.fullBranch) // push current branch to the same ref on origin
        cmd.call()
        Unit
    }

    /** The current HEAD commit SHA. */
    suspend fun head(): String = withContext(Dispatchers.IO) {
        git.repository.resolve(Constants.HEAD)?.name ?: error("head: no HEAD")
    }

    /** Releases the JGit handles (open files / pack locks). Idempotent; safe to call from `use {}`. */
    override fun close() = git.close()

    companion object {
        /**
         * Clones [url] into [dir] (which must not already exist). A non-empty [token] is used
         * as GitHub HTTP auth (x-access-token).
         */
        suspend fun clone(url: String, dir: String, token: String): Repo = withContext(Dispatchers.IO) {
            val cred: CredentialsProvider? =
                if (token.isEmpty()) null else UsernamePasswordCredentialsProvider("x-access-token", token)
            val git = Git.cloneRepository()
                .setURI(url)
                .setDirectory(File(dir))
                .apply { cred?.let { setCredentialsProvider(it) } }
                .call()
            Repo(git, File(dir), cred, Instant::now)
        }
    }
}
