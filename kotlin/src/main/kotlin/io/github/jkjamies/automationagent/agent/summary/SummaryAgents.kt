package io.github.jkjamies.automationagent.agent.summary

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.agents.ParallelAgent
import com.google.adk.kt.agents.SequentialAgent
import com.google.adk.kt.events.Event
import com.google.adk.kt.models.Model
import io.github.jkjamies.automationagent.agent.setup.Prompts
import io.github.jkjamies.automationagent.agent.setup.generateText
import io.github.jkjamies.automationagent.agent.setup.textEvent
import io.github.jkjamies.automationagent.notify.Message
import io.github.jkjamies.automationagent.notify.Notifier
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.time.Duration
import java.time.Instant

private val prompts = Prompts.forAgent("summary")

/**
 * Injected dependencies for the summary workflow. [llm], [gh] and [notifier] are non-null by type,
 * so the "required deps" validation is a compile-time guarantee here.
 */
data class SummaryDeps(
    val llm: Model,
    val gh: CommitLister,
    val notifier: Notifier,
    val repos: List<String>, // owner/repo entries; one parallel fetcher each
    val window: Duration = Duration.ofHours(24),
    val title: String = "Daily commit digest", // notification heading
    val now: () -> Instant = { Instant.now() },
)

/**
 * Wires the summary workflow:
 *
 *     Sequential[ Parallel[fetch×N] -> summarizeAndNotify ]
 *
 * Fetchers write per-repo commit data to state; [SummarizeAndNotifyAgent] reads it, summarizes via
 * the LLM, and posts the digest. (ADK-Kotlin has no `OutputKey`, so — rather than an `LlmAgent` +
 * a separate notify agent threading state through `digest` — the summarize and notify steps are
 * one code agent that calls `generateText` directly.)
 */
fun buildSummaryAgent(deps: SummaryDeps): BaseAgent {
    require(deps.repos.isNotEmpty()) { "summary: at least one repo is required" }

    val fetchers = deps.repos.map { FetchAgent(it, deps.gh, deps.window, deps.now) }
    val parallel =
        ParallelAgent(
            name = "fetch_all",
            description = "Fetches recent commits for all configured repositories",
            subAgents = fetchers,
        )
    val summarizeNotify = SummarizeAndNotifyAgent(deps.llm, deps.notifier, prompts.get("summarize"), deps.title)
    return SequentialAgent(
        name = "summary_workflow",
        description = "Commit digest workflow",
        subAgents = listOf(parallel, summarizeNotify),
    )
}

/**
 * Reads the per-repo commit data the fetchers wrote to state, summarizes it via the LLM, and posts
 * the digest to chat. Collapses the summarizer (LlmAgent + OutputKey) and `notify` code agent into
 * one node (see [buildSummaryAgent]).
 */
internal class SummarizeAndNotifyAgent(
    private val llm: Model,
    private val notifier: Notifier,
    private val promptBody: String,
    private val title: String,
) : BaseAgent(name = "summarize_notify", description = "Summarizes recent commits and posts the digest") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        val instruction = buildInstruction(promptBody, context.session.state)
        val digest = generateText(llm, instruction, "Summarize the recent commits.").trim().ifEmpty { "(no digest was produced)" }
        notifier.notify(Message(title = title, text = digest))
        emit(textEvent(name, "Posted digest to chat."))
    }
}
