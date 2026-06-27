package com.automation.agent.agent.fixflow

import com.google.adk.kt.agents.Instruction
import com.google.adk.kt.agents.LlmAgent
import com.google.adk.kt.sessions.InMemorySessionService
import com.google.adk.kt.tools.BaseTool
import com.google.adk.kt.tools.ToolContext
import com.google.adk.kt.types.FunctionDeclaration
import com.google.adk.kt.types.Schema
import com.google.adk.kt.types.Type
import com.automation.agent.agent.setup.DriveResult
import com.automation.agent.agent.setup.LongRunDriver
import com.automation.agent.agent.setup.MemoryParkStore
import com.automation.agent.agent.setup.ParkRecord
import com.automation.agent.agent.setup.ParkStore
import com.automation.agent.agent.setup.SequencerConfig
import com.automation.agent.agent.setup.newSequencerModel
import com.automation.agent.githubapi.Comparison
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.time.Instant
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap
import kotlin.time.Duration

private const val TOOL_APPLY_FIX = "apply_fix"
private const val TOOL_AWAIT_CI = "await_ci"

private val paramsJson = Json { encodeDefaults = true; ignoreUnknownKeys = true }

/**
 * The per-run inputs the apply_fix tool needs. Owned by the Driver (serialized into the park record,
 * keyed by session id) and never model-controlled, so a misbehaving model cannot redirect which repo
 * or branch is edited. [feedback] and [newBranch] are folded in on retry via a copy.
 */
@Serializable
internal data class RunParams(
    val owner: String,
    val repo: String,
    val fullRepo: String,
    val base: String,
    val report: String,
    val feedback: String = "",
    val newBranch: Boolean = false,
)

internal fun runParamsToJson(rp: RunParams): String = paramsJson.encodeToString(RunParams.serializer(), rp)

internal fun runParamsFromJson(s: String): RunParams = paramsJson.decodeFromString(RunParams.serializer(), s)

/** The normalized resume context derived from a check_run webhook. */
data class ResumeInput(
    val fullRepo: String,
    val prNumber: Int,
    val conclusion: String,
    val outputText: String,
)

/**
 * Runs a Spec's CI-wait loop on ADK's resumability suspend/resume, backed by a [ParkStore]. It owns
 * the long-run agent and all policy — retry vs give up, attempt counting, the per-run timeout —
 * while the suspended-run state lives in the (memory or durable) store. The agent's sequencer model
 * only emits a fixed apply_fix→await_ci sequence.
 *
 * Lifecycle: kickoff applies a fix and parks on await_ci (a [ParkRecord] indexed by PR key). A
 * check_run webhook drives resume, which atomically claims the run and either notifies success,
 * resumes for another attempt, or gives up at maxIter. If CI never reports, a soft per-run timer
 * fires onTimeout; a durable catch-all, [sweepTimeouts] (driven by the periodic /internal/sweep),
 * resolves runs whose timer was lost to a restart. The store's single-winner claim guarantees
 * exactly one of those paths resolves a given run.
 */
class Driver private constructor(private val engine: Engine) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    /** The durable (or in-memory) parked-run store. Exposed for tests to inspect/seed. */
    val store: ParkStore = engine.deps.parkStore ?: MemoryParkStore()

    // Soft per-run timers keyed by PR key. A restart loses these; sweepTimeouts is the durable
    // catch-all that frees runs whose timer was dropped.
    private val timers = ConcurrentHashMap<String, Job>()
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
                        // A clean apply (triage found nothing) is already terminal: conclude without
                        // parking on CI so the result is never forwarded to await_ci.
                        stopWhen = { isClean(it) },
                    ),
                )
            val fixer =
                LlmAgent(
                    name = "fixer-${engine.spec.name}",
                    model = seqModel,
                    instruction = Instruction("Apply the fix, then wait for CI. If CI fails, apply again."),
                    tools = listOf(dr.ApplyFixTool(), dr.AwaitCiTool()),
                )
            dr.lr = LongRunDriver.create(
                appName = "fixflow-${engine.spec.name}",
                userId = "fixer",
                root = fixer,
                sessionService = engine.deps.sessionService ?: InMemorySessionService(),
            )
            return dr
        }
    }

    // --- tools -------------------------------------------------------------

    /**
     * Applies one fix attempt for the calling session. The run params are loaded from the store by
     * session id (Driver-owned), so the model's args cannot influence the target. Self-wraps
     * failures as `{"error": …}` so the sequencer's apply-error branch can conclude.
     */
    private inner class ApplyFixTool : BaseTool(name = TOOL_APPLY_FIX, description = "Apply the fix, commit it, and open or update the PR.") {
        override fun declaration(): FunctionDeclaration =
            FunctionDeclaration(name = name, description = description, parameters = Schema(type = Type.OBJECT, properties = emptyMap()))

        override suspend fun run(context: ToolContext, args: Map<String, Any>): Any {
            val sid = context.invocationContext.session.key.id ?: return mapOf("error" to "apply_fix: no session id")
            return try {
                val rec = store.get(sid) ?: return mapOf("error" to "apply_fix: no run params for session \"$sid\"")
                val res = engine.attemptOnce(runParamsFromJson(rec.params))
                mapOf("pr_number" to res.pr.number, "head_sha" to res.headSha)
            } catch (e: NoWorkException) {
                // Triage found nothing actionable — not a failure. Report a clean result so the
                // sequencer concludes (stopWhen) and afterDrive sends a positive notice.
                mapOf("clean" to true)
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

    // --- lifecycle ---------------------------------------------------------

    /** Starts a new suspended run: apply the fix, then park awaiting CI. */
    suspend fun kickoff(k: Kickoff) {
        val sid = newSessionId()
        putParams(sid, RunParams(owner = k.owner(), repo = k.name(), fullRepo = k.repo, base = k.base, report = k.reportText(), newBranch = true))
        val res =
            try {
                lr.start(sid, "Apply the fix and wait for CI.")
            } catch (e: Exception) {
                clear(sid)
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
            store.resolveByPrKey(key) ?: run {
                engine.log.log(System.Logger.Level.INFO, "resume: no parked run workflow=${engine.spec.name} pr=$key conclusion=${input.conclusion}")
                return
            }
        stopTimer(key) // the webhook won; cancel the soft timer for this run

        if (input.conclusion == "success") {
            clear(run.sessionId)
            terminalNotify(TerminalOutcome.SUCCESS, engine.spec.successTitle, run, input.fullRepo, input.prNumber, "")
            return
        }

        // failure
        if (run.attempts >= engine.maxIter) {
            clear(run.sessionId)
            terminalNotify(TerminalOutcome.EXHAUSTED, engine.spec.reviewTitle, run, input.fullRepo, input.prNumber, input.outputText)
            return
        }

        val res =
            try {
                // Persist the retry params and resume under one guard: if updateForRetry fails after
                // the record was claimed, clear() frees the run so it doesn't linger claimed-but-stuck.
                updateForRetry(run.sessionId, input.outputText)
                lr.resume(run.sessionId, run.callId, TOOL_AWAIT_CI, mapOf("conclusion" to input.conclusion, "output" to input.outputText))
            } catch (e: Exception) {
                clear(run.sessionId)
                throw e
            }
        afterDrive(run.sessionId, input.fullRepo, res, run.attempts + 1)
    }

    /** Fires (from the soft timer) when a parked run's CI never reports: frees it, asks review. */
    internal suspend fun onTimeout(key: String) {
        timers.remove(key) // the timer has fired; drop its handle
        val run = store.resolveByPrKey(key) ?: return // already resolved by a webhook or the sweep
        clear(run.sessionId)
        val (fullRepo, pr) = splitPrKey(key)
        engine.log.log(System.Logger.Level.WARNING, "fix timed out waiting for CI workflow=${engine.spec.name} repo=$fullRepo pr=$pr")
        terminalNotify(TerminalOutcome.TIMEOUT, engine.spec.reviewTitle, run, fullRepo, pr, "")
    }

    /**
     * Durable timeout catch-all: free every parked run whose CI never reported, driven by the
     * periodic /internal/sweep. Covers runs whose soft timer was lost to a restart. Each run is
     * claimed exactly once by the store, so this never races the webhook or the timer.
     */
    suspend fun sweepTimeouts() {
        val cutoff = Instant.now().minusMillis(engine.ciTimeout.inWholeMilliseconds)
        for (run in store.sweep(cutoff)) {
            stopTimer(run.prKey)
            clear(run.sessionId)
            val (fullRepo, pr) = splitPrKey(run.prKey)
            engine.log.log(System.Logger.Level.WARNING, "fix swept; CI never reported workflow=${engine.spec.name} repo=$fullRepo pr=$pr")
            terminalNotify(TerminalOutcome.TIMEOUT, engine.spec.reviewTitle, run, fullRepo, pr, "")
        }
    }

    /** The number of currently parked runs (test/inspection utility). */
    suspend fun parkedCount(): Int = store.parkedCount()

    /**
     * Builds and sends the status-aware summary for a finished run: the outcome framing, the original
     * targeted findings, and what actually changed on the PR (best-effort; a decode/compare failure
     * still sends the attempt count + framing).
     */
    private suspend fun terminalNotify(outcome: TerminalOutcome, title: String, run: ParkRecord, fullRepo: String, prNumber: Int, lastOutput: String) {
        var report = ""
        var changed = Comparison()
        try {
            val rp = runParamsFromJson(run.params)
            report = rp.report
            changed = gatherChanges(rp)
        } catch (e: Exception) {
            engine.log.log(System.Logger.Level.WARNING, "decode run params for summary failed workflow=${engine.spec.name} session=${run.sessionId}: ${e.message}")
        }
        val input =
            SummaryInput(
                outcome = outcome, workflow = engine.spec.name, fullRepo = fullRepo, prNumber = prNumber,
                attempts = run.attempts, report = report, lastOutput = lastOutput,
                timeout = formatTimeout(engine.ciTimeout), checkName = engine.spec.checkName, changed = changed,
            )
        engine.notify(title, buildSummaryText(input), pullUrl(fullRepo, prNumber))
    }

    /**
     * Best-effort fetch of the PR branch's base...head diff for a terminal summary. On error returns
     * an empty comparison so the summary still reports the attempt count and findings.
     */
    private suspend fun gatherChanges(rp: RunParams): Comparison =
        try {
            engine.deps.gh.compare(rp.owner, rp.repo, rp.base, engine.spec.branch)
        } catch (e: Exception) {
            engine.log.log(System.Logger.Level.WARNING, "compare for summary failed workflow=${engine.spec.name} repo=${rp.fullRepo}: ${e.message}")
            Comparison()
        }

    // --- internals ---------------------------------------------------------

    /** Surfaces an apply error or parks the run (and its timeout) under its PR key. */
    private suspend fun afterDrive(sid: String, fullRepo: String, res: DriveResult, attempt: Int) {
        val apply = res.toolResponses[TOOL_APPLY_FIX]
        if (apply != null && apply.containsKey("error")) {
            fail(sid, fullRepo, "the fix could not be applied: ${apply["error"]}")
        }
        if (isClean(apply)) {
            finishClean(sid, fullRepo)
            return
        }
        val parkedCallId = res.parkedCallId
        if (parkedCallId == null) {
            fail(sid, fullRepo, "run did not park on CI wait")
        }
        val pr = prNumberFrom(apply)
        if (pr == 0) {
            fail(sid, fullRepo, "parked without a PR number")
        }
        park(sid, prKey(fullRepo, pr), parkedCallId, attempt)
        engine.log.log(System.Logger.Level.INFO, "fix applied; awaiting CI workflow=${engine.spec.name} repo=$fullRepo pr=$pr attempt=$attempt")
    }

    /** Stores a fresh run's params (not yet parked: empty PR key, zero attempts). */
    private suspend fun putParams(sid: String, rp: RunParams) {
        store.put(ParkRecord(sessionId = sid, params = runParamsToJson(rp)))
    }

    /** Records a run as parked under its PR key and arms its soft timeout. */
    private suspend fun park(sid: String, key: String, callId: String, attempt: Int) {
        val rec = store.get(sid)
        if (rec == null) {
            // The run vanished mid-flight (e.g. a concurrent clear) — nothing to park.
            engine.log.log(System.Logger.Level.WARNING, "park: no run params; skipping workflow=${engine.spec.name} pr=$key")
            return
        }
        store.put(rec.copy(prKey = key, callId = callId, attempts = attempt, parkedAt = Instant.now()))
        armTimer(key)
    }

    /**
     * Clears the run, notifies a human that the fix needs review, and throws. A failed apply (no PR,
     * push rejected, analyze error) is terminal: without this, the error would only be logged at the
     * dispatch edge and the run would vanish with no notification.
     */
    private suspend fun fail(sid: String, fullRepo: String, reason: String): Nothing {
        clear(sid)
        val msg = "$fullRepo ${engine.spec.name}: $reason"
        engine.notify(engine.spec.reviewTitle, "$msg. Please review.", "")
        throw RuntimeException(msg)
    }

    /**
     * Resolves a run whose triage found nothing to address. No PR was opened and the run never parked,
     * so it just frees the run and sends a positive "already clean" notice — never the human-review
     * alarm. Returns normally so the dispatcher does not log a no-op as a failure.
     */
    private suspend fun finishClean(sid: String, fullRepo: String) {
        engine.log.log(System.Logger.Level.INFO, "nothing to address; already clean workflow=${engine.spec.name} repo=$fullRepo")
        clear(sid)
        val text = buildSummaryText(SummaryInput(outcome = TerminalOutcome.CLEAN, workflow = engine.spec.name, fullRepo = fullRepo, prNumber = 0, attempts = 0))
        engine.notify(engine.spec.cleanTitle, text, "")
    }

    private fun newSessionId(): String = UUID.randomUUID().toString()

    /**
     * Folds the failed attempt's CI output back into the stored run params as feedback (and forces a
     * reuse of the existing branch) so the next resume re-runs the analyze step with that context.
     */
    private suspend fun updateForRetry(sid: String, feedback: String) {
        val rec = store.get(sid) ?: return
        val rp = runParamsFromJson(rec.params).copy(feedback = "The previous attempt failed CI with:\n$feedback", newBranch = false)
        store.put(rec.copy(params = runParamsToJson(rp)))
    }

    /**
     * Terminal cleanup: drop the park record and the ADK session. Best-effort — errors are logged
     * but never unwind the caller, so a failed cleanup cannot strand a resolution.
     */
    private suspend fun clear(sid: String) {
        try {
            store.delete(sid)
        } catch (e: Exception) {
            engine.log.log(System.Logger.Level.WARNING, "clear: park-record delete failed workflow=${engine.spec.name} session=$sid: ${e.message}")
        }
        try {
            lr.deleteSession(sid)
        } catch (e: Exception) {
            engine.log.log(System.Logger.Level.WARNING, "clear: session delete failed workflow=${engine.spec.name} session=$sid: ${e.message}")
        }
    }

    /**
     * (Re)arms the in-process soft timer for a parked PR: if CI never reports within the timeout,
     * onTimeout frees the run. A durable /internal/sweep backstops it across restarts.
     */
    private fun armTimer(key: String) {
        timers.remove(key)?.cancel() // replace any prior timer for this PR (e.g. a retry re-park)
        timers[key] = scope.launch {
            delay(engine.ciTimeout)
            onTimeout(key)
        }
    }

    /** Cancels and forgets the soft timer for a PR once it has been resolved or swept. */
    private fun stopTimer(key: String) {
        timers.remove(key)?.cancel()
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

/**
 * Whether an apply result is the clean sentinel (triage found nothing). Tolerant of the value being a
 * Boolean or its string round-trip through the ADK function-response transport.
 */
internal fun isClean(resp: Map<String, Any?>?): Boolean =
    when (val v = resp?.get("clean")) {
        is Boolean -> v
        else -> v?.toString()?.toBoolean() ?: false
    }

/** Formats a duration as a compact human string (e.g. `90m`, `1h`, `30s`) for the timeout summary. */
internal fun formatTimeout(d: Duration): String {
    val ms = d.inWholeMilliseconds
    return when {
        ms > 0 && ms % 3_600_000L == 0L -> "${ms / 3_600_000L}h"
        ms > 0 && ms % 60_000L == 0L -> "${ms / 60_000L}m"
        ms > 0 && ms % 1_000L == 0L -> "${ms / 1_000L}s"
        else -> "${ms}ms"
    }
}
