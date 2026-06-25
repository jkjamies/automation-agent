package io.github.jkjamies.automationagent.ingest

import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import java.time.Instant

class EnvelopeTest : BehaviorSpec({
    Given("the recognized ingest kinds") {
        When("checking validity") {
            Then("each known kind is valid") {
                listOf(Kind.CRON_DAILY, Kind.LINT, Kind.COVERAGE, Kind.CI)
                    .forEach { Kind.valid(it.value) shouldBe true }
            }
            Then("an unknown kind is invalid") {
                Kind.valid("jira") shouldBe false
            }
        }
    }

    Given("a kind, source, payload and timestamp") {
        val at = Instant.ofEpochSecond(1718870400)
        When("constructing an Envelope") {
            val e = Envelope.new(Kind.LINT, "webhook:/lint", "{\"x\":1}".toByteArray(), at)
            Then("the fields are preserved") {
                e.kind shouldBe Kind.LINT
                e.source shouldBe "webhook:/lint"
                String(e.payload) shouldBe "{\"x\":1}"
                e.receivedAt shouldBe at
            }
        }
    }
})
