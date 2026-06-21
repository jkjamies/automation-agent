package io.github.jkjamies.automationagent.agent.fixflow

import com.google.adk.kt.agents.Instruction
import com.google.adk.kt.agents.LlmAgent
import com.google.adk.kt.tools.BaseTool
import com.google.adk.kt.tools.ToolContext
import com.google.adk.kt.types.FunctionDeclaration
import com.google.adk.kt.types.Schema
import com.google.adk.kt.types.Type
import io.github.jkjamies.automationagent.agent.setup.DriveResult
import io.github.jkjamies.automationagent.agent.setup.LongRunDriver
import io.github.jkjamies.automationagent.agent.setup.SequencerConfig
import io.github.jkjamies.automationagent.agent.setup.newSequencerModel
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

private const val TOOL_APPLY_FIX = "apply_fix"
private const val TOOL_AWAIT_CI = "await_ci"

/**
 * The per-run inputs the apply_fix tool needs. Owned by the Driver (keyed by session id) and never
 * model-controlled, so a misbehaving model cannot redirect which repo or branch is edited.
 * [feedback] and [newBranch] are mutated on retry.
 */
internal class RunParams(
    val owner: String,
    val repo: String,
    val fullRepo: String,
    val base: String,
    val report: String,
    var feedback: String = "",
    var newBranch: Boolean = false,
)

/** The normalized resume context derived from a check_run webhook. */
data class ResumeInput(
    val fullRepo: String,
    val prNumber: Int,
    val conclusion: String,
    val outputText: String,
)

/**
 * Runs a Spec's CI-wait loop on ADK's resumability suspend/resume. It owns the long-run agent, the
 * in-memory parked-run registry, and each session's run params. All policy — retry vs give up,
 * attempt counting, the per-run timeout — lives here; the agent's sequencer model only emits a
 * fixed apply_fix→await_ci sequence. There is no durable store: a process restart strands parked
 * runs (an accepted trade).
 */
class Driver private constructor(private val engine: Engine) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    internal val reg = RunRegistry(scope)
    private val runs = ConcurrentHashMap<String, RunParams>()
    private val seq = AtomicLong(0)
    private lateinit var lr: LongRunDriver

    companion object {
        fun create(engine: Engine): Driver {
            val dr = Driver(engine)
            val seqModel =
                newSequencerModel(
                    SequencerConfig(
                        action = TOOL_APPLY_FIX,
                        wait = TOOL_AWAIT_CI,
                        // The Driver only resumes a run when it has already decided to retry, so a
                        // resumed failure always means "apply again". (success/timeout never resume.)
                        retryWhen = { it["conclusion"]?.toString() == "failure" },
                    ),
                )
            val fixer =
                LlmAgent(
                    name = "fixer-${engine.spec.name}",
                    model = seqModel,
                    instruction = Instruction("Apply the fix, then wait for CI. If CI fails, apply again."),
                    tools = listOf(dr.ApplyFixTool(), dr.AwaitCiTool()),
                )
            dr.lr = LongRunDriver.create("fixflow-${engine.spec.name}", "fixer", fixer)
            return dr
        }
    }

    /** Applies one fix attempt for the calling session. Self-wraps failures as `{"error": …}`. */
    private inner class ApplyFixTool : BaseTool(name = TOOL_APPLY_FIX, description = "Apply the fix, commit it, and open or update the PR.") {
        override fun declaration(): FunctionDeclaration =
            FunctionDeclaration(name = name, description = description, parameters = Schema(type = Type.OBJECT, properties = emptyMap()))

        override suspend fun run(context: ToolContext, args: Map<String, Any>): Any {
            val sid = context.invocationContext.session.key.id ?: return mapOf("error" to "apply_fix: no session id")
            val rp = runs[sid] ?: return mapOf("error" to "apply_fix: no run params for session \"$sid\"")
            return try {
                val res = engine.attemptOnce(rp)
                mapOf("pr_number" to res.pr.number, "head_sha" to res.headSha)
            } catch (e: Exception) {
                mapOf("error" to (e.message ?: e.toString()))
            }
        }
    }

    /** The long-running park point: records that the run is waiting and returns a pending status. */
    private inner class AwaitCiTool : BaseTool(name = TOOL_AWAIT_CI, description = "Wait for CI to report on the PR. Returns a pending status, then the real result later.", isLongRunning = true) {
        override fun declaration(): FunctionDeclaration =
            FunctionDeclaration(
                name = name,
                description = description,
                parameters = Schema(type = Type.OBJECT, properties = mapOf("pr_number" to Schema(type = Type.INTEGER), "head_sha" to Schema(type = Type.STRING))),
            )

        override suspend fun run(context: ToolContext, args: Map<String, Any>): Any = mapOf("status" to "pending")
    }

    /** Starts a new suspended run: apply the fix, then park awaiting CI. */
    suspend fun kickoff(k: Kickoff) {
        val sid = newSessionId()
        runs[sid] = RunParams(owner = k.owner(), repo = k.name(), fullRepo = k.repo, base = k.base, report = k.reportText(), newBranch = true)
        val res =
            try {
                lr.start(sid, "Apply the fix and wait for CI.")
            } catch (e: Exception) {
                runs.remove(sid)
                throw e
            }
        afterDrive(sid, k.repo, res, 1)
    }

    /** Reacts to a CI conclusion for a parked run. */
    suspend fun resume(input: ResumeInput) {
        require(input.prNumber != 0) { "resume: missing PR number" }
        // Only success/failure are actionable. Anything else leaves the run parked for a later
        // conclusive event (or the timeout) to resolve.
        if (input.conclusion != "success" && input.conclusion != "failure") {
            engine.log.log(System.Logger.Level.INFO, "ignoring non-actionable conclusion workflow=${engine.spec.name} repo=${input.fullRepo} conclusion=${input.conclusion}")
            return
        }

        val key = prKey(input.fullRepo, input.prNumber)
        val run =
            reg.resolve(key) ?: run {
                engine.log.log(System.Logger.Level.INFO, "resume: no parked run workflow=${engine.spec.name} pr=$key conclusion=${input.conclusion}")
                return
            }
        val link = pullUrl(input.fullRepo, input.prNumber)

        if (input.conclusion == "success") {
            clear(run.sessionId)
            engine.notify(engine.spec.successTitle, "${input.fullRepo}: ${engine.spec.name} passed CI.", link)
            return
        }

        // failure
        if (run.attempts >= engine.maxIter) {
            clear(run.sessionId)
            engine.notify(engine.spec.reviewTitle, "${input.fullRepo}: after ${run.attempts} attempts the ${engine.spec.name} fix still fails CI. Please review.", link)
            return
        }

        updateForRetry(run.sessionId, input.outputText)
        val res =
            try {
                lr.resume(run.sessionId, run.callId, TOOL_AWAIT_CI, mapOf("conclusion" to input.conclusion, "output" to input.outputText))
            } catch (e: Exception) {
                clear(run.sessionId)
                throw e
            }
        afterDrive(run.sessionId, input.fullRepo, res, run.attempts + 1)
    }

    /** Fires (from the registry timer) when a parked run's CI never reports: frees it, asks review. */
    internal suspend fun onTimeout(key: String) {
        val run = reg.resolve(key) ?: return // already resolved by a webhook
        clear(run.sessionId)
        val (fullRepo, pr) = splitPrKey(key)
        engine.log.log(System.Logger.Level.WARNING, "fix timed out waiting for CI workflow=${engine.spec.name} repo=$fullRepo pr=$pr")
        engine.notify(engine.spec.reviewTitle, "$fullRepo: the ${engine.spec.name} fix timed out after ${engine.ciTimeout} waiting for CI. Please review.", pullUrl(fullRepo, pr))
    }

    /** Surfaces an apply error or parks the run (and its timeout) under its PR key. */
    private fun afterDrive(sid: String, fullRepo: String, res: DriveResult, attempt: Int) {
        val apply = res.toolResponses[TOOL_APPLY_FIX]
        if (apply != null && apply.containsKey("error")) {
            clear(sid)
            throw RuntimeException("$fullRepo ${engine.spec.name}: ${apply["error"]}")
        }
        val parkedCallId = res.parkedCallId
        if (parkedCallId == null) {
            clear(sid)
            throw RuntimeException("$fullRepo ${engine.spec.name}: run did not park on CI wait")
        }
        val pr = prNumberFrom(apply)
        if (pr == 0) {
            clear(sid)
            throw RuntimeException("$fullRepo ${engine.spec.name}: parked without a PR number")
        }
        reg.park(prKey(fullRepo, pr), ParkedRun(sessionId = sid, callId = parkedCallId, attempts = attempt), engine.ciTimeout, ::onTimeout)
        engine.log.log(System.Logger.Level.INFO, "fix applied; awaiting CI workflow=${engine.spec.name} repo=$fullRepo pr=$pr attempt=$attempt")
    }

    private fun newSessionId(): String = "run-${seq.incrementAndGet()}"

    private fun updateForRetry(sid: String, feedback: String) {
        runs[sid]?.let {
            it.feedback = "The previous attempt failed CI with:\n$feedback"
            it.newBranch = false
        }
    }

    private fun clear(sid: String) {
        runs.remove(sid)
    }
}

internal fun prKey(fullRepo: String, number: Int): String = "$fullRepo#$number"

internal fun splitPrKey(key: String): Pair<String, Int> {
    val repo = key.substringBefore('#')
    val number = key.substringAfter('#', "").toIntOrNull() ?: 0
    return repo to number
}

internal fun prNumberFrom(resp: Map<String, Any?>?): Int =
    resp?.get("pr_number")?.toString()?.toDoubleOrNull()?.toInt() ?: 0
