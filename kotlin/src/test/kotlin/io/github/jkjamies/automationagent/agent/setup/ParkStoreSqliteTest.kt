package io.github.jkjamies.automationagent.agent.setup

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.collections.shouldHaveSize
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import java.io.File
import java.time.Instant

private fun tempDbPath(): String = File.createTempFile("parkstore", ".db").apply { delete() }.absolutePath

private fun parked(sessionId: String, prKey: String, callId: String = "c", attempts: Int = 0, parkedAt: Instant? = Instant.now()): ParkRecord =
    ParkRecord(sessionId = sessionId, prKey = prKey, callId = callId, attempts = attempts, params = """{"x":1}""", parkedAt = parkedAt)

class ParkStoreSqliteTest : BehaviorSpec({
    Given("a sqlite park store") {
        When("a record is stored and the store is reopened on the same file") {
            Then("the record survives (durability across a 'restart')") {
                val path = tempDbPath()
                val s1 = SqliteParkStore(path)
                s1.put(parked("sess", "o/r#1", callId = "call", attempts = 2))
                s1.close()

                val s2 = SqliteParkStore(path)
                val rec = s2.get("sess").shouldNotBeNull()
                rec.prKey shouldBe "o/r#1"
                rec.callId shouldBe "call"
                rec.attempts shouldBe 2
                rec.params shouldBe """{"x":1}"""
                s2.parkedCount() shouldBe 1
                s2.close()
            }
        }

        When("resolving a parked run twice by PR key") {
            Then("the first wins and the second finds nothing") {
                val s = SqliteParkStore(tempDbPath())
                s.put(parked("sess", "o/r#1"))
                val first = s.resolveByPrKey("o/r#1").shouldNotBeNull()
                first.prKey shouldBe "o/r#1"
                s.resolveByPrKey("o/r#1") shouldBe null
                s.parkedCount() shouldBe 0
                s.get("sess").shouldNotBeNull().prKey shouldBe "" // retained, unparked
                s.close()
            }
        }

        When("an empty or unknown key is resolved") {
            Then("it is a no-op") {
                val s = SqliteParkStore(tempDbPath())
                s.resolveByPrKey("") shouldBe null
                s.resolveByPrKey("never#9") shouldBe null
                s.close()
            }
        }

        When("a session re-parks under a new PR key") {
            Then("the old key no longer resolves and only the new one does") {
                val s = SqliteParkStore(tempDbPath())
                s.put(parked("sess", "o/r#1"))
                s.put(parked("sess", "o/r#2", attempts = 1))
                s.resolveByPrKey("o/r#1") shouldBe null
                s.resolveByPrKey("o/r#2").shouldNotBeNull().attempts shouldBe 1
                s.close()
            }
        }

        When("sweeping with runs parked before and after a cutoff") {
            Then("only the stale ones are claimed, each exactly once") {
                val s = SqliteParkStore(tempDbPath())
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
            Then("it and its index are gone") {
                val s = SqliteParkStore(tempDbPath())
                s.put(parked("sess", "o/r#1"))
                s.delete("sess")
                s.get("sess") shouldBe null
                s.resolveByPrKey("o/r#1") shouldBe null
                s.close()
            }
        }
    }
})
