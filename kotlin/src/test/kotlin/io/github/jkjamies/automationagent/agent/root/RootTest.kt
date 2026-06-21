package io.github.jkjamies.automationagent.agent.root

import com.google.adk.kt.agents.BaseAgent
import com.google.adk.kt.agents.InvocationContext
import com.google.adk.kt.events.Event
import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import java.time.Instant

private fun env(kind: Kind): Envelope = Envelope.new(kind, "test", ByteArray(0), Instant.ofEpochSecond(1))

/** A code agent that emits one event — a real runner without an LLM. */
private class TrivialAgent : BaseAgent(name = "trivial") {
    override fun runAsyncImpl(context: InvocationContext): Flow<Event> = flow {
        emit(Event(author = "trivial"))
    }
}

class RootTest : BehaviorSpec({
    Given("a dispatcher with a registered cron handler") {
        When("dispatching that kind") {
            Then("it routes to the handler") {
                val d = Dispatcher()
                var got: Kind? = null
                d.register(Kind.CRON_DAILY) { e -> got = e.kind }
                d.handles(Kind.CRON_DAILY) shouldBe true
                d.dispatch(env(Kind.CRON_DAILY))
                got shouldBe Kind.CRON_DAILY
            }
        }
    }

    Given("a dispatcher with no handler for a kind") {
        When("dispatching it") {
            Then("it is a silent no-op") {
                val d = Dispatcher()
                d.handles(Kind.LINT) shouldBe false
                d.dispatch(env(Kind.LINT)) // must not throw
            }
        }
    }

    Given("a handler that throws") {
        When("dispatching its kind") {
            Then("the error propagates") {
                val d = Dispatcher()
                d.register(Kind.CI) { throw RuntimeException("handler failed") }
                shouldThrow<RuntimeException> { d.dispatch(env(Kind.CI)) }
            }
        }
    }

    Given("a root dispatcher built with a summary agent") {
        When("inspecting and dispatching cron kinds") {
            Then("cron kinds route to the summary workflow") {
                val d = buildRootDispatcher(RootDeps(summaryAgent = TrivialAgent()))
                d.handles(Kind.CRON_DAILY) shouldBe true
                d.handles(Kind.CRON_WEEKLY) shouldBe true
                d.dispatch(env(Kind.CRON_DAILY)) // drives a real runner, must not throw
            }
        }
    }

    Given("a root dispatcher built with fix handlers") {
        When("dispatching lint, coverage and ci") {
            Then("each registered handler is invoked") {
                val called = mutableSetOf<Kind>()
                val mark = Handler { e -> called += e.kind }
                val d = buildRootDispatcher(RootDeps(lintKickoff = mark, coverageKickoff = mark, ciResume = mark))
                d.handles(Kind.LINT) shouldBe true
                d.handles(Kind.COVERAGE) shouldBe true
                d.handles(Kind.CI) shouldBe true
                for (k in listOf(Kind.LINT, Kind.COVERAGE, Kind.CI)) d.dispatch(env(k))
                called shouldBe setOf(Kind.LINT, Kind.COVERAGE, Kind.CI)
            }
        }
    }

    Given("a root dispatcher built without a summary agent") {
        When("checking cron kinds") {
            Then("they are unhandled") {
                val d = buildRootDispatcher(RootDeps(summaryAgent = null))
                d.handles(Kind.CRON_DAILY) shouldBe false
            }
        }
    }
})
