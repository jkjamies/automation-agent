package com.automation.agent.ingest

import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import java.time.Instant

class EnvelopeCodecTest : BehaviorSpec({
    Given("the canonical envelope from the cross-port wire contract") {
        val e = Envelope.new(Kind.LINT, "webhook:/lint", "hi".toByteArray(), Instant.EPOCH)
        When("encoding it") {
            val bytes = String(encode(e))
            Then("the bytes are byte-identical to the pinned contract") {
                bytes shouldBe
                    """{"kind":"lint","source":"webhook:/lint","received_at":"1970-01-01T00:00:00Z","payload":"aGk="}"""
            }
        }
    }

    Given("an envelope with an empty payload") {
        val e = Envelope.new(Kind.CRON_DAILY, "internal:/cron/daily", ByteArray(0), Instant.EPOCH)
        When("encoding it") {
            val bytes = String(encode(e))
            Then("the payload is the empty string, never null or []") {
                bytes shouldBe
                    """{"kind":"cron.daily","source":"internal:/cron/daily","received_at":"1970-01-01T00:00:00Z","payload":""}"""
            }
        }
    }

    Given("an envelope") {
        val e = Envelope.new(Kind.CI, "webhook:/github", "{\"action\":\"completed\"}".toByteArray(), Instant.ofEpochSecond(1_718_870_400))
        When("round-tripping through encode then decode") {
            val back = decode(encode(e))
            Then("every field survives") {
                back shouldBe e
            }
        }
    }

    Given("a sub-second timestamp") {
        When("encoding an instant with a half-second fraction") {
            val e = Envelope.new(Kind.LINT, "s", ByteArray(0), Instant.ofEpochSecond(0, 500_000_000))
            Then("the trailing fractional zeros are trimmed (Go RFC3339Nano spelling)") {
                String(encode(e)) shouldBe
                    """{"kind":"lint","source":"s","received_at":"1970-01-01T00:00:00.5Z","payload":""}"""
            }
        }
        When("encoding an instant with microsecond precision") {
            val e = Envelope.new(Kind.LINT, "s", ByteArray(0), Instant.ofEpochSecond(0, 123_456_000))
            Then("only the trailing zeros are trimmed") {
                String(encode(e)) shouldBe
                    """{"kind":"lint","source":"s","received_at":"1970-01-01T00:00:00.123456Z","payload":""}"""
            }
        }
    }

    Given("a wire body with an absent source and received_at") {
        When("decoding it") {
            val e = decode("""{"kind":"lint","payload":"aGk="}""")
            Then("they default to the zero value rather than failing") {
                e.source shouldBe ""
                e.receivedAt shouldBe Instant.EPOCH
                String(e.payload) shouldBe "hi"
            }
        }
    }

    Given("a wire body with JSON-null source and received_at") {
        When("decoding it") {
            val e = decode("""{"kind":"lint","source":null,"received_at":null,"payload":"aGk="}""")
            Then("the nulls coerce to the zero value") {
                e.source shouldBe ""
                e.receivedAt shouldBe Instant.EPOCH
            }
        }
    }

    Given("a wire body with an absent or JSON-null payload") {
        // Go's wireEnvelope.Payload (a string, defaulting to its "" zero value) and the JS
        // `w.payload ?? ''` both decode an absent or JSON-null payload to the empty string — i.e. an
        // empty payload, never poison. Only a *non-string* payload (covered below) is poison. Pin
        // this boundary so the cross-port "empty payload = empty string" rule cannot regress.
        listOf(
            "absent" to """{"kind":"lint","source":"s"}""",
            "JSON null" to """{"kind":"lint","source":"s","payload":null}""",
        ).forEach { (name, body) ->
            When("decoding a $name payload") {
                val e = decode(body)
                Then("it defaults to an empty payload, not poison") {
                    e.payload.size shouldBe 0
                }
            }
        }
    }

    Given("a wire body with an unknown extra key") {
        When("decoding it") {
            val e = decode("""{"kind":"lint","source":"s","payload":"aGk=","extra":true}""")
            Then("the extra key is ignored, not poison") {
                e.kind shouldBe Kind.LINT
            }
        }
    }

    Given("malformed wire bodies") {
        listOf(
            "not JSON" to "not json at all",
            "a JSON array" to """["lint"]""",
            "an unknown kind" to """{"kind":"jira","payload":""}""",
            "junk base64 payload" to """{"kind":"lint","payload":"not!base64"}""",
            "non-canonical base64 (missing padding)" to """{"kind":"lint","payload":"aGk"}""",
            "a non-string payload" to """{"kind":"lint","payload":123}""",
            "a non-string source" to """{"kind":"lint","source":123,"payload":""}""",
            "an unparseable received_at" to """{"kind":"lint","received_at":"not-a-date","payload":""}""",
            "a date-only received_at" to """{"kind":"lint","received_at":"1970-01-01","payload":""}""",
        ).forEach { (name, body) ->
            When("decoding $name") {
                Then("it is rejected as a permanent poison error") {
                    shouldThrow<IllegalArgumentException> { decode(body) }
                }
            }
        }
    }
})
