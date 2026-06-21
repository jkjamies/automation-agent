package io.github.jkjamies.automationagent.scheduler

import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.nulls.shouldNotBeNull
import io.kotest.matchers.shouldBe
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.withTimeout
import java.time.Instant

class SchedulerTest : BehaviorSpec({
    Given("valid and invalid cron specs") {
        When("adding them") {
            Then("valid specs register and an invalid spec throws") {
                val s = Scheduler({ })
                s.add("0 9 * * *", Kind.CRON_DAILY)
                s.add("0 9 * * 1", Kind.CRON_WEEKLY)
                s.entries() shouldBe 2
                shouldThrow<IllegalArgumentException> { s.add("not a cron spec", Kind.CRON_DAILY) }
            }
        }
    }

    Given("an @every schedule") {
        When("started") {
            Then("it fires and can be stopped") {
                val fired = CompletableDeferred<Kind>()
                val s = Scheduler({ e -> fired.complete(e.kind) })
                s.add("@every 1s", Kind.CRON_DAILY)
                s.start()
                try {
                    val kind = withTimeout(3000) { fired.await() }
                    kind shouldBe Kind.CRON_DAILY
                } finally {
                    s.stop()
                }
            }
        }
    }

    Given("a fixed clock") {
        When("triggering directly") {
            Then("it emits a scheduler envelope with the clock's timestamp") {
                var captured: Envelope? = null
                val fixed = Instant.ofEpochSecond(1718870400)
                val s = Scheduler({ e -> captured = e }, now = { fixed })
                s.trigger(Kind.CRON_WEEKLY)
                val got = captured.shouldNotBeNull()
                got.kind shouldBe Kind.CRON_WEEKLY
                got.source shouldBe "scheduler"
                got.receivedAt shouldBe fixed
            }
        }
    }
})
