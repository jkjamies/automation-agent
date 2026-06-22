/*
 * Package summary builds the daily commit-digest workflow: parallel fetchers write per-repo commit
 * data to session state, an LLM summarizes it, and the digest is posted to chat.
 */
package io.github.jkjamies.automationagent.agent.summary

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.events.Event
import io.github.jkjamies.automationagent.agent.setup.safeName
import io.github.jkjamies.automationagent.agent.setup.textEvent
import io.github.jkjamies.automationagent.githubapi.Commit
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.time.Duration
import java.time.Instant

/** The slice of `githubapi` the fetchers need (consumer-defined for fakeability). */
fun interface CommitLister {
    suspend fun listCommitsSince(owner: String, repo: String, since: Instant): List<Commit>
}

internal const val STATE_PREFIX = "commits:" // one key per repo: commits:<owner/repo>
internal const val DIGEST_KEY = "digest" // summarizer output

/**
 * A code agent that fetches the last [window] of commits for [repo] and writes a formatted digest
 * to state under `commits:<repo>`. Implemented as a custom [BaseAgent].
 */
internal class FetchAgent(
    private val repo: String,
    private val gh: CommitLister,
    private val window: Duration,
    private val now: () -> Instant,
) : BaseAgent(name = "fetch_${safeName(repo)}", description = "Fetches recent commits for $repo") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        val (owner, name2) = splitRepo(repo) ?: throw IllegalArgumentException("invalid repo \"$repo\" (want owner/repo)")
        val commits = gh.listCommitsSince(owner, name2, now().minus(window))
        val text = formatCommits(repo, commits)
        emit(textEvent(name, text, mapOf(STATE_PREFIX + repo to text)))
    }
}

/**
 * Assembles the summarizer instruction: the prompt body followed by the per-repo commit data the
 * fetchers wrote to state, sorted by key. Non-`commits:` state keys are ignored.
 */
internal fun buildInstruction(promptBody: String, state: Map<String, Any?>): String {
    val items =
        state.entries
            .filter { it.key.startsWith(STATE_PREFIX) }
            .mapNotNull { e -> (e.value as? String)?.let { e.key to it } }
            .sortedBy { it.first }
    return buildString {
        append(promptBody)
        append("\n\n## Commits\n")
        if (items.isEmpty()) append("(no commit data)\n")
        items.forEach { append(it.second); append("\n") }
    }
}

internal fun formatCommits(repo: String, commits: List<Commit>): String {
    if (commits.isEmpty()) return "Repository $repo: no commits in the window."
    return buildString {
        append("Repository $repo (${commits.size} commits):\n")
        commits.forEach { append("- ${shortSha(it.sha)} ${firstLine(it.message)} (${it.author})\n") }
    }
}

internal fun firstLine(s: String): String {
    val i = s.indexOf('\n')
    return if (i >= 0) s.substring(0, i).trim() else s.trim()
}

internal fun shortSha(sha: String): String = if (sha.length > 7) sha.substring(0, 7) else sha

/** Splits "owner/repo" into its parts, or null if malformed. */
internal fun splitRepo(s: String): Pair<String, String>? {
    val owner = s.substringBefore('/', "")
    val repo = s.substringAfter('/', "")
    return if (owner.isEmpty() || repo.isEmpty() || !s.contains('/')) null else owner to repo
}
