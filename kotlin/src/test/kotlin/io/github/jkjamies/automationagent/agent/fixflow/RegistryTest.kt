package io.github.jkjamies.automationagent.agent.fixflow

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout
import java.util.concurrent.atomic.AtomicInteger
import kotlin.time.Duration.Companion.hours
import kotlin.time.Duration.Companion.milliseconds

private fun newRegistry(): RunRegistry = RunRegistry(CoroutineScope(SupervisorJob() + Dispatchers.Default))

private val noTimeout: suspend (String) -> Unit = { }

class RegistryTest : BehaviorSpec({
    Given("a parked run") {
        When("resolving it twice") {
            Then("the first wins and the second finds nothing") {
                val r = newRegistry()
                r.park("o/r#1", ParkedRun("s", "c"), 1.hours, noTimeout)
                r.size() shouldBe 1
                r.resolve("o/r#1").shouldNotBeNull().callId shouldBe "c"
                r.resolve("o/r#1") shouldBe null
                r.size() shouldBe 0
            }
        }
    }

    Given("an unparked PR") {
        When("resolving it") {
            Then("it is a no-op") {
                newRegistry().resolve("never/parked#9") shouldBe null
            }
        }
    }

    Given("a parked run whose timer fires") {
        When("the timeout claims it") {
            Then("a late webhook afterward finds nothing") {
                val r = newRegistry()
                val claimed = CompletableDeferred<Boolean>()
                r.park("o/r#2", ParkedRun("s", "c"), 10.milliseconds) { pr -> claimed.complete(r.resolve(pr) != null) }
                withTimeout(2000) { claimed.await() } shouldBe true
                r.resolve("o/r#2") shouldBe null // late webhook after the timeout claimed it
            }
        }
    }

    Given("many racers resolving one parked run") {
        When("they contend") {
            Then("exactly one wins") {
                val r = newRegistry()
                r.park("o/r#3", ParkedRun("s", "c"), 1.hours, noTimeout)
                val wins = AtomicInteger(0)
                coroutineScope {
                    repeat(50) { launch { if (r.resolve("o/r#3") != null) wins.incrementAndGet() } }
                }
                wins.get() shouldBe 1
            }
        }
    }

    Given("a re-park under the same PR key") {
        When("resolving") {
            Then("only the latest parking is resolvable") {
                val r = newRegistry()
                r.park("o/r#4", ParkedRun("s", "c1", attempts = 1), 1.hours, noTimeout)
                r.park("o/r#4", ParkedRun("s", "c2", attempts = 2), 1.hours, noTimeout)
                r.size() shouldBe 1
                val run = r.resolve("o/r#4").shouldNotBeNull()
                run.callId shouldBe "c2"
                run.attempts shouldBe 2
            }
        }
    }
})
