package io.github.jkjamies.automationagent.agent.setup

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.events.Event
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow

/** A minimal custom agent that emits a single text event with a state delta. */
private class EchoAgent : BaseAgent(name = "echo") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        emit(textEvent("echo", "hello", mapOf("k" to "v")))
    }
}

/** An agent whose run throws, to prove the driver propagates the failure. */
private class BoomAgent : BaseAgent(name = "boom") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        throw RuntimeException("kaboom")
    }
}

class RunnerTest : BehaviorSpec({
    Given("an echo agent behind an in-memory runner") {
        When("driving it") {
            Then("the run completes without error") {
                val r = newRunner("test-app", EchoAgent())
                drive(r, "u", "s", "go")
            }
        }
        When("collecting state") {
            Then("the emitted state delta is accumulated") {
                val r = newRunner("test-app", EchoAgent())
                driveCollectState(r, "u", "s", "go")["k"] shouldBe "v"
            }
        }
    }

    Given("an agent that errors") {
        When("driving it") {
            Then("the error propagates") {
                val r = newRunner("test-app", BoomAgent())
                shouldThrow<RuntimeException> { drive(r, "u", "s", "go") }
            }
        }
    }
})
