package com.automation.agent.agent.setup

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch
import java.time.Instant
import java.util.concurrent.atomic.AtomicInteger

private fun parked(sessionId: String, prKey: String, callId: String = "c", attempts: Int = 0, parkedAt: Instant? = Instant.now()): ParkRecord =
    ParkRecord(sessionId = sessionId, prKey = prKey, callId = callId, attempts = attempts, params = "{}", parkedAt = parkedAt)

class ParkStoreTest : BehaviorSpec({
    Given("a stored record") {
        When("getting it by session id") {
            Then("the record is returned and reflects its fields") {
                val s = MemoryParkStore()
                s.put(parked("sess", "o/r#1", callId = "call", attempts = 2))
                val rec = s.get("sess").shouldNotBeNull()
                rec.prKey shouldBe "o/r#1"
                rec.callId shouldBe "call"
                rec.attempts shouldBe 2
                s.parkedCount() shouldBe 1
            }
        }
    }

    Given("a parked run") {
        When("resolving it twice by PR key") {
            Then("the first wins (with the claimed key) and the second finds nothing") {
                val s = MemoryParkStore()
                s.put(parked("sess", "o/r#1", callId = "c"))
                val first = s.resolveByPrKey("o/r#1").shouldNotBeNull()
                first.callId shouldBe "c"
                first.prKey shouldBe "o/r#1" // the claimed key is handed back
                s.resolveByPrKey("o/r#1") shouldBe null
                s.parkedCount() shouldBe 0
                // The record is retained (unparked) so a retry can still read its params.
                s.get("sess").shouldNotBeNull().prKey shouldBe ""
            }
        }
    }

    Given("an empty or unknown PR key") {
        When("resolving") {
            Then("it is a no-op") {
                val s = MemoryParkStore()
                s.resolveByPrKey("") shouldBe null
                s.resolveByPrKey("never/parked#9") shouldBe null
            }
        }
    }

    Given("a re-park of the same session under a new PR key") {
        When("resolving the old key") {
            Then("the stale index is dropped and only the new key resolves") {
                val s = MemoryParkStore()
                s.put(parked("sess", "o/r#1"))
                s.put(parked("sess", "o/r#2", attempts = 1))
                s.resolveByPrKey("o/r#1") shouldBe null
                s.resolveByPrKey("o/r#2").shouldNotBeNull().attempts shouldBe 1
            }
        }
    }

    Given("runs parked before and after a cutoff") {
        When("sweeping") {
            Then("only the stale ones are claimed, each exactly once") {
                val s = MemoryParkStore()
                val cutoff = Instant.now()
                s.put(parked("old", "o/r#1", parkedAt = cutoff.minusSeconds(60)))
                s.put(parked("fresh", "o/r#2", parkedAt = cutoff.plusSeconds(60)))
                val swept = s.sweep(cutoff)
                swept shouldHaveSize 1
                swept[0].prKey shouldBe "o/r#1"
                s.parkedCount() shouldBe 1 // the fresh one is still parked
            }
        }
    }

    Given("a stored record") {
        When("deleting it") {
            Then("the record and its index are gone") {
                val s = MemoryParkStore()
                s.put(parked("sess", "o/r#1"))
                s.delete("sess")
                s.get("sess") shouldBe null
                s.resolveByPrKey("o/r#1") shouldBe null
                s.parkedCount() shouldBe 0
            }
        }
    }

    Given("many racers resolving one parked run") {
        When("they contend") {
            Then("exactly one wins") {
                val s = MemoryParkStore()
                s.put(parked("sess", "o/r#3"))
                val wins = AtomicInteger(0)
                coroutineScope {
                    repeat(50) { launch { if (s.resolveByPrKey("o/r#3") != null) wins.incrementAndGet() } }
                }
                wins.get() shouldBe 1
            }
        }
    }
})
