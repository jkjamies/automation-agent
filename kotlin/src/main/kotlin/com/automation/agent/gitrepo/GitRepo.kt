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
import org.eclipse.jgit.api.TransportConfigCallback
import org.eclipse.jgit.lib.Constants
import org.eclipse.jgit.lib.PersonIdent
import org.eclipse.jgit.transport.CredentialsProvider
import org.eclipse.jgit.transport.SshTransport
import org.eclipse.jgit.transport.UsernamePasswordCredentialsProvider
import org.eclipse.jgit.transport.sshd.JGitKeyCache
import org.eclipse.jgit.transport.sshd.SshdSessionFactory
import org.eclipse.jgit.transport.sshd.SshdSessionFactoryBuilder
import java.io.File
import java.time.Instant
import java.time.ZoneId

/** Identifies the committer. */
data class Author(val name: String, val email: String)

/**
 * Yields a valid GitHub token for a repo (`"owner/name"`), re-fetched per git op. The gitrepo-local
 * view of `auth.TokenProvider` (a narrow interface kept here so gitrepo stays decoupled from the
 * `auth` package; the composition root adapts the real provider to it).
 */
fun interface TokenProvider {
    suspend fun token(repo: String): String
}

/**
 * Credentials [Repo.clone] / [Repo.push] use. Which one applies is chosen by the clone URL scheme,
 * not by the caller: an https remote uses [provider] (GitHub `x-access-token` transport auth,
 * re-fetched per op so a short-lived installation token stays current), an ssh remote
 * (`git@…` / `ssh://…`) uses [sshKey] or the ssh-agent.
 */
data class Auth(
    // provider yields the token supplied as x-access-token transport auth on https remotes, fetched
    // fresh per git op (scoped to [repo]). null — or a token of "" — means anonymous (public read
    // only). Ignored for ssh remotes.
    val provider: TokenProvider? = null,
    // repo is "owner/name", passed to provider so App mode can scope the token.
    val repo: String = "",
    // sshKey is an explicit private-key path for ssh remotes; empty falls back to the ssh-agent then
    // default identities. Ignored for https remotes.
    val sshKey: String = "",
)

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
    // The clean clone URL (no embedded credential) and auth, kept so [push] can re-resolve a fresh
    // token per op — GitHub App installation tokens are short-lived (~1h), so a token captured at
    // clone time may be stale by push.
    private val url: String,
    private val auth: Auth,
    // SSH transport factory for an ssh remote (ssh-agent/keys + known_hosts); null for https.
    // Built on clone and reused by push; closed in [close] to release its key cache / SSH client.
    private val sshFactory: SshdSessionFactory?,
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

    /**
     * Pushes the current branch to origin. An up-to-date push is not an error. The https token is
     * re-resolved here (not reused from clone) so a fresh, repo-scoped token authenticates the push
     * even if the clone-time token has since expired; JGit supplies it as in-memory transport auth,
     * so the credential never lands in `.git/config` (matching the Go reference).
     */
    suspend fun push() = withContext(Dispatchers.IO) {
        val cmd = git.push()
        if (sshFactory != null) {
            cmd.setTransportConfigCallback(sshConfigCallback(sshFactory)) // ssh remote
        } else {
            httpsCred(tokenFor(url, auth))?.let { cmd.setCredentialsProvider(it) } // https remote
        }
        cmd.add(git.repository.fullBranch) // push current branch to the same ref on origin
        cmd.call()
        Unit
    }

    /** The current HEAD commit SHA. */
    suspend fun head(): String = withContext(Dispatchers.IO) {
        git.repository.resolve(Constants.HEAD)?.name ?: error("head: no HEAD")
    }

    /** Releases the JGit handles (open files / pack locks). Idempotent; safe to call from `use {}`. */
    override fun close() {
        git.close()
        sshFactory?.close() // releases the SSH key cache / client (no-op for https clones)
    }

    companion object {
        /**
         * Clones [url] into [dir] (which must not already exist). Auth is chosen by the URL scheme:
         * an https remote resolves a token from [Auth.provider] and supplies it as GitHub
         * `x-access-token` transport auth; an ssh remote (`git@…` / `ssh://…`) ignores the provider
         * and uses ssh-agent / [Auth.sshKey] / the default identity files, with `known_hosts`
         * verification on. A plaintext `http://` remote is refused (token leak). The token is given as
         * in-memory transport auth, never embedded in the remote URL, so it never lands in
         * `.git/config` (matching the Go reference).
         */
        suspend fun clone(url: String, dir: String, auth: Auth = Auth()): Repo =
            withContext(Dispatchers.IO) {
                if (isSshUrl(url)) {
                    val factory = buildSshFactory(auth.sshKey)
                    // The factory's key cache / SSH client is released by Repo.close(); if the clone
                    // itself fails the Repo is never built, so close the factory here to avoid a leak.
                    val git = try {
                        Git.cloneRepository()
                            .setURI(url)
                            .setDirectory(File(dir))
                            .setTransportConfigCallback(sshConfigCallback(factory))
                            .call()
                    } catch (e: Throwable) {
                        factory.close()
                        throw e
                    }
                    Repo(git, File(dir), url, auth, sshFactory = factory, now = Instant::now)
                } else {
                    // Resolve a token for an https remote (refuses http://; local paths → none) and
                    // supply it as transport auth — never written to the remote URL / .git/config.
                    val cred = httpsCred(tokenFor(url, auth))
                    val git = Git.cloneRepository()
                        .setURI(url)
                        .setDirectory(File(dir))
                        .apply { cred?.let { setCredentialsProvider(it) } }
                        .call()
                    Repo(git, File(dir), url, auth, sshFactory = null, now = Instant::now)
                }
            }
    }
}

/** Whether [url] is an scp-style (`git@host:path`) or `ssh://` remote rather than https. */
internal fun isSshUrl(url: String): Boolean = url.startsWith("git@") || url.startsWith("ssh://")

/**
 * Resolves the token for an https git op, fetched fresh per op so a short-lived installation token
 * stays current. Returns `""` (anonymous) for ssh / local / file remotes, which need no token —
 * fetching one would needlessly mint a GitHub installation token in App mode. Throws for a plaintext
 * `http://` remote: sending a token as basic auth over an unencrypted transport would leak it.
 */
internal suspend fun tokenFor(url: String, auth: Auth): String {
    if (isSshUrl(url)) return ""
    if (url.startsWith("http://")) {
        throw IllegalArgumentException("refusing to send GitHub token over insecure http remote; use https or ssh")
    }
    if (!url.startsWith("https://")) return "" // local path / file:// — no credentials.
    val provider = auth.provider ?: return ""
    return provider.token(auth.repo)
}

/** The in-memory https transport credential for a non-empty token, or null (anonymous). */
private fun httpsCred(token: String): CredentialsProvider? =
    if (token.isEmpty()) null else UsernamePasswordCredentialsProvider("x-access-token", token)

/** Attaches [factory] to JGit's SSH transport for a clone/push command. */
private fun sshConfigCallback(factory: SshdSessionFactory): TransportConfigCallback =
    TransportConfigCallback { transport ->
        if (transport is SshTransport) transport.sshSessionFactory = factory
    }

/**
 * Builds an OpenSSH-mirroring [SshdSessionFactory] (Apache MINA sshd) rooted at the user's home so
 * it reads `~/.ssh` and verifies host keys against `~/.ssh/known_hosts` (verification stays ON — the
 * server-key database is never overridden). Key resolution mirrors the `ssh` binary: an explicit
 * [sshKey] (GIT_SSH_KEY) wins and is used as the sole identity (no agent); otherwise a running
 * ssh-agent is preferred, then the default `~/.ssh` identity files.
 */
internal fun buildSshFactory(sshKey: String): SshdSessionFactory {
    val home = File(System.getProperty("user.home"))
    val builder = SshdSessionFactoryBuilder()
        .setHomeDirectory(home)
        .setSshDirectory(File(home, ".ssh"))
    if (sshKey.isNotEmpty()) {
        // Explicit key wins: authenticate with exactly this identity file and disable the agent,
        // mirroring `ssh -i <key>`. known_hosts verification is unaffected.
        val keyPath = File(sshKey).toPath()
        builder.setConnectorFactory(null) // no ssh-agent
            .setDefaultIdentities { listOf(keyPath) }
    } else {
        // No explicit key: prefer a running ssh-agent (SSH_AUTH_SOCK), then the default ~/.ssh
        // identity files — the ssh binary's own resolution order.
        builder.withDefaultConnectorFactory()
    }
    return builder.build(JGitKeyCache())
}
