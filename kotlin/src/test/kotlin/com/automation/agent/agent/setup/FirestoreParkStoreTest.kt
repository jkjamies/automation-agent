package com.automation.agent.agent.setup

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import java.time.Instant

// Emulator-gated: runs only when FIRESTORE_EMULATOR_HOST is set (start one with
// `gcloud beta emulators firestore start --host-port=localhost:8080`). Excluded from the coverage
// floor (see build.gradle.kts). Mirrors ParkStoreSqliteTest against the cloud backend.
private val emulatorEnabled = System.getenv("FIRESTORE_EMULATOR_HOST") != null

private fun store(): FirestoreParkStore = FirestoreParkStore("demo-test", "parked_${System.nanoTime()}")

private fun parked(sessionId: String, prKey: String, callId: String = "c", attempts: Int = 0, parkedAt: Instant? = Instant.now()): ParkRecord =
    ParkRecord(sessionId = sessionId, prKey = prKey, callId = callId, attempts = attempts, params = """{"x":1}""", parkedAt = parkedAt)

class FirestoreParkStoreTest : BehaviorSpec({
    Given("a firestore park store") {
        When("a record is stored and read back") {
            Then("the fields round-trip").config(enabled = emulatorEnabled) {
                val s = store()
                s.put(parked("sess", "o/r#1", callId = "call", attempts = 2))
                val rec = s.get("sess").shouldNotBeNull()
                rec.prKey shouldBe "o/r#1"
                rec.callId shouldBe "call"
                rec.attempts shouldBe 2
                s.parkedCount() shouldBe 1
                s.close()
            }
        }

        When("resolving a parked run twice by PR key") {
            Then("the first wins and the second finds nothing").config(enabled = emulatorEnabled) {
                val s = store()
                s.put(parked("sess", "o/r#1"))
                s.resolveByPrKey("o/r#1").shouldNotBeNull().prKey shouldBe "o/r#1"
                s.resolveByPrKey("o/r#1") shouldBe null
                s.parkedCount() shouldBe 0
                s.get("sess").shouldNotBeNull().prKey shouldBe "" // retained, unparked
                s.close()
            }
        }

        When("an empty or unknown key is resolved") {
            Then("it is a no-op").config(enabled = emulatorEnabled) {
                val s = store()
                s.resolveByPrKey("") shouldBe null
                s.resolveByPrKey("never#9") shouldBe null
                s.close()
            }
        }

        When("sweeping with runs parked before and after a cutoff") {
            Then("only the stale ones are claimed").config(enabled = emulatorEnabled) {
                val s = store()
                val cutoff = Instant.now()
                s.put(parked("old", "o/r#1", parkedAt = cutoff.minusSeconds(60)))
                s.put(parked("fresh", "o/r#2", parkedAt = cutoff.plusSeconds(60)))
                val swept = s.sweep(cutoff)
                swept shouldHaveSize 1
                swept[0].prKey shouldBe "o/r#1"
                s.parkedCount() shouldBe 1
                s.close()
            }
        }

        When("a record is deleted") {
            Then("it is gone").config(enabled = emulatorEnabled) {
                val s = store()
                s.put(parked("sess", "o/r#1"))
                s.delete("sess")
                s.get("sess") shouldBe null
                s.resolveByPrKey("o/r#1") shouldBe null
                s.close()
            }
        }
    }
})
