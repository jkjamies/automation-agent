package com.automation.agent.agent.setup

import com.google.adk.kt.agents.Instruction
import com.google.adk.kt.agents.LlmAgent
import com.google.adk.kt.tools.BaseTool
import com.google.adk.kt.tools.ToolContext
import com.google.adk.kt.types.Content
import com.google.adk.kt.types.FunctionDeclaration
import com.google.adk.kt.types.FunctionResponse
import com.google.adk.kt.types.Part
import com.google.adk.kt.types.Schema
import com.google.adk.kt.types.Type
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import io.kotest.matchers.shouldNotBe
import io.kotest.matchers.string.shouldContain
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

/** `apply` performs the work; it self-wraps failures as `{"error": …}` (the tool-error contract). */
private class ApplyTool(private val calls: AtomicInteger, private val fail: AtomicBoolean) :
    BaseTool(name = "apply", description = "apply a fix") {
    override fun declaration(): FunctionDeclaration =
        FunctionDeclaration(name = name, description = description, parameters = Schema(type = Type.OBJECT, properties = emptyMap()))

    override suspend fun run(context: ToolContext, args: Map<String, Any>): Any {
        calls.incrementAndGet()
        return if (fail.get()) mapOf("error" to "apply boom") else mapOf("pr_number" to 7, "head_sha" to "abc")
    }
}

/** `await` is the long-running tool: it returns a pending payload and parks the run. */
private class AwaitTool : BaseTool(name = "await", description = "await CI", isLongRunning = true) {
    override fun declaration(): FunctionDeclaration =
        FunctionDeclaration(
            name = name,
            description = description,
            parameters =
                Schema(
                    type = Type.OBJECT,
                    properties = mapOf("pr_number" to Schema(type = Type.INTEGER), "head_sha" to Schema(type = Type.STRING)),
                ),
        )

    override suspend fun run(context: ToolContext, args: Map<String, Any>): Any = mapOf("status" to "pending")
}

private fun ciFailure(response: Map<String, Any?>): Boolean = response["conclusion"]?.toString() == "failure"

private fun buildDriver(calls: AtomicInteger, fail: AtomicBoolean): LongRunDriver {
    val agent =
        LlmAgent(
            name = "lr",
            model = newSequencerModel(SequencerConfig(action = "apply", wait = "await", retryWhen = ::ciFailure)),
            instruction = Instruction("apply then await"),
            tools = listOf(ApplyTool(calls, fail), AwaitTool()),
        )
    return LongRunDriver.create("lr-app", "u", agent)
}

private fun Map<String, Any?>.asInt(key: String): Int = getValue(key).toString().toDouble().toInt()

class LongrunTest : BehaviorSpec({
    Given("the sequencer decision logic") {
        val s = Sequencer(SequencerConfig(action = "apply", wait = "await", retryWhen = ::ciFailure))
        fun fcName(r: com.google.adk.kt.models.LlmResponse): String? =
            r.content?.parts?.firstNotNullOfOrNull { it.functionCall?.name }
        fun text(r: com.google.adk.kt.models.LlmResponse): String =
            r.content?.parts?.mapNotNull { it.text }?.joinToString("") ?: ""
        fun history(name: String, body: Map<String, Any?>): Content =
            Content(parts = listOf(Part(functionResponse = FunctionResponse(name = name, response = body))))

        When("driving over crafted histories") {
            Then("it sequences apply -> await -> conclude, with retry on failure") {
                fcName(s.decide(emptyList())) shouldBe "apply"
                fcName(s.decide(listOf(history("apply", mapOf("pr_number" to 7))))) shouldBe "await"

                val applyErr = s.decide(listOf(history("apply", mapOf("error" to "x"))))
                fcName(applyErr) shouldBe null
                text(applyErr) shouldContain "failed"

                fcName(s.decide(listOf(history("await", mapOf("conclusion" to "failure"))))) shouldBe "apply"

                val awaitOk = s.decide(listOf(history("await", mapOf("conclusion" to "success"))))
                fcName(awaitOk) shouldBe null
                text(awaitOk) shouldNotBe ""
            }
        }
    }

    Given("a long-running apply/await agent") {
        When("driving start -> resume(failure) -> resume(success)") {
            Then("apply runs once per attempt and the loop concludes") {
                val calls = AtomicInteger(0)
                val d = buildDriver(calls, AtomicBoolean(false))

                val start = d.start("s1", "go")
                val startId = start.parkedCallId.shouldNotBeNull()
                start.toolResponses.getValue("apply").asInt("pr_number") shouldBe 7
                calls.get() shouldBe 1

                val retry = d.resume("s1", startId, "await", mapOf("conclusion" to "failure"))
                val retryId = retry.parkedCallId.shouldNotBeNull()
                retryId shouldNotBe startId
                calls.get() shouldBe 2

                val done = d.resume("s1", retryId, "await", mapOf("conclusion" to "success"))
                done.parkedCallId shouldBe null
                calls.get() shouldBe 2
                done.final shouldContain "done"
            }
        }

        When("apply fails") {
            Then("the run surfaces the error and does not park") {
                val d = buildDriver(AtomicInteger(0), AtomicBoolean(true))
                val res = d.start("s1", "go")
                res.parkedCallId shouldBe null
                res.toolResponses.getValue("apply").containsKey("error") shouldBe true
                res.final shouldContain "failed"
            }
        }

        When("CI times out, then a late webhook replays the stale call id") {
            Then("the timeout concludes the run and the late replay is a benign no-op") {
                val d = buildDriver(AtomicInteger(0), AtomicBoolean(false))
                val start = d.start("s2", "go")
                val parkedId = start.parkedCallId.shouldNotBeNull()

                // A non-failure outcome (timeout) concludes without re-parking.
                val timedOut = d.resume("s2", parkedId, "await", mapOf("conclusion" to "timeout"))
                timedOut.parkedCallId shouldBe null

                // A late webhook replays the now-stale call id — it must not re-park or error.
                val late = d.resume("s2", parkedId, "await", mapOf("conclusion" to "success"))
                late.parkedCallId shouldBe null
            }
        }
    }
})
