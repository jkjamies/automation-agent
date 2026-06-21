package io.github.jkjamies.automationagent.agent.fixflow

import com.google.adk.kt.models.Model
import io.github.jkjamies.automationagent.githubapi.Client
import io.github.jkjamies.automationagent.gitrepo.Author
import io.github.jkjamies.automationagent.notify.Message
import io.github.jkjamies.automationagent.notify.Notifier
import java.io.File
import kotlin.time.Duration
import kotlin.time.Duration.Companion.minutes

/** One file and the items to address in it (lint problems, uncovered regions, …). */
data class FileWork(val path: String, val items: List<String> = emptyList())

/** Normalizes an arbitrary tool report into per-file work (LLM-backed). */
fun interface TriageFunc {
    suspend operator fun invoke(llm: Model?, report: String): List<FileWork>
}

/**
 * What an [AnalyzeFunc] receives. [repoDir] is the checked-out working tree: analyze reads source
 * from it (and may explore it), and the engine commits whatever edits are returned from the same
 * checkout. [llm] is the default model (planning/exploration); [codeLlm] is the (often larger)
 * model for writing code.
 */
data class AnalyzeInput(
    val llm: Model?,
    val codeLlm: Model?,
    val repoDir: String,
    val work: List<FileWork>,
    val feedback: String, // previous attempt's CI failure, on retry
) {
    /** The code-change model, falling back to the default model when no dedicated one is set. */
    fun coder(): Model? = codeLlm ?: llm
}

/** Produces the whole-file edits to apply (rewritten source, new tests, …). */
fun interface AnalyzeFunc {
    suspend operator fun invoke(input: AnalyzeInput): List<FileEdit>
}

/** The per-workflow configuration that turns the engine into a concrete fixing agent. */
data class Spec(
    val name: String, // "lint" | "coverage"
    val branch: String, // e.g. automation-agent/lint-fix
    val label: String, // e.g. automation-agent
    val checkName: String, // e.g. agent-lint-verify
    val commitMessage: String,
    val prTitle: String,
    val successTitle: String, // notification title on success
    val reviewTitle: String, // notification title when human review is needed
    val triage: TriageFunc,
    val analyze: AnalyzeFunc,
)

private val defaultAuthor = Author("automation-agent", "automation-agent@users.noreply.github.com")

/**
 * Runtime dependencies shared by all engines. [codeLlm] is the model for code-change steps
 * (typically larger); it falls back to [llm] when null. [ciTimeout] bounds how long a single
 * suspended run waits for its CI result before it is freed. [cloneUrl] is overridable in tests.
 */
data class Deps(
    val gh: GitHub,
    val llm: Model? = null,
    val codeLlm: Model? = null,
    val notifier: Notifier? = null,
    val token: String = "",
    val maxIter: Int = 3,
    val ciTimeout: Duration = 90.minutes,
    val author: Author = defaultAuthor,
    val log: System.Logger = System.getLogger("automation-agent.fixflow"),
    val cloneUrl: ((owner: String, repo: String) -> String)? = null,
)

/**
 * Runs one Spec's event-driven fix loop. The CI-wait suspend/resume itself is owned by the
 * [Driver] (ADK resumability + an in-memory parked-run registry). Effective dependency values
 * (defaults applied) are exposed for the Driver.
 */
class Engine(val spec: Spec, val deps: Deps) {
    internal val maxIter: Int = if (deps.maxIter <= 0) 3 else deps.maxIter
    internal val ciTimeout: Duration = if (deps.ciTimeout <= Duration.ZERO) 90.minutes else deps.ciTimeout
    internal val author: Author = if (deps.author.name.isEmpty()) defaultAuthor else deps.author
    internal val codeLlm: Model? = deps.codeLlm ?: deps.llm
    internal val log: System.Logger = deps.log

    internal val driver: Driver = Driver.create(this)

    /** The PR label this engine's workflow uses. */
    fun label(): String = spec.label

    /** The agent verify check this engine resumes on. */
    fun checkName(): String = spec.checkName

    /** Handles a kickoff envelope: starts a suspended fix run (apply → await CI). */
    suspend fun kickoff(raw: ByteArray) {
        val k = parseKickoff(raw)
        log.log(System.Logger.Level.INFO, "fix kickoff workflow=${spec.name} repo=${k.repo}")
        driver.kickoff(k)
    }

    /**
     * Handles a GitHub check_run webhook. No-ops unless the event is this engine's check completing
     * — so multiple engines can each be handed the event.
     */
    suspend fun resume(raw: ByteArray) {
        val ev = Client.parseCheckRunEvent(raw)
        if (ev.checkName != spec.checkName || ev.status != "completed") return
        driver.resume(ResumeInput(fullRepo = ev.repoFullName, prNumber = ev.prNumber, conclusion = ev.conclusion, outputText = ev.outputText))
    }

    /**
     * Runs a single fix attempt against [rp]: triage → checkout → analyze → commit, returning the
     * resulting PR. The body the apply_fix tool invokes; the surrounding suspend/retry loop lives in
     * the Driver. One checkout is shared by analyze (read/explore) and commit (write/push).
     */
    internal suspend fun attemptOnce(rp: RunParams): ApplyResult {
        val work = spec.triage(deps.llm, rp.report)
        val cfg =
            ApplyConfig(
                owner = rp.owner, repo = rp.repo, cloneUrl = cloneUrl(rp.owner, rp.repo), token = deps.token,
                base = rp.base, branch = spec.branch, newBranch = rp.newBranch, label = spec.label,
                commitMessage = spec.commitMessage, prTitle = spec.prTitle, prBody = prBody(spec, work), author = author,
            )
        val repo = open(cfg)
        return try {
            val edits = spec.analyze(AnalyzeInput(llm = deps.llm, codeLlm = codeLlm, repoDir = repo.dir(), work = work, feedback = rp.feedback))
            commit(deps.gh, repo, cfg, edits)
        } finally {
            File(repo.dir()).deleteRecursively()
        }
    }

    internal suspend fun notify(title: String, text: String, link: String) {
        deps.notifier?.notify(Message(title = title, text = text, link = link))
    }

    internal fun cloneUrl(owner: String, repo: String): String =
        deps.cloneUrl?.invoke(owner, repo) ?: "https://github.com/$owner/$repo.git"
}

internal fun pullUrl(fullRepo: String, number: Int): String = "https://github.com/$fullRepo/pull/$number"

internal fun prBody(spec: Spec, work: List<FileWork>): String =
    buildString {
        append("Automated ${spec.name} fix by automation-agent.\n\nFiles addressed:\n")
        work.forEach { append("- `${it.path}` (${it.items.size} item(s))\n") }
    }
