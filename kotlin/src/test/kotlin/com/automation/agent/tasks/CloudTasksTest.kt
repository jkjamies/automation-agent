package com.automation.agent.tasks

import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind
import com.automation.agent.ingest.encode
import com.google.cloud.tasks.v2.CreateTaskRequest
import com.google.cloud.tasks.v2.HttpMethod
import com.google.cloud.tasks.v2.Task
import io.kotest.assertions.throwables.shouldThrow
import io.kotest.core.spec.style.BehaviorSpec
import io.kotest.matchers.shouldBe
import java.time.Instant
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

private const val QUEUE = "projects/p/locations/l/queues/q"
private const val URL = "https://agent.example.run.app/internal/dispatch"

/** Captures the last CreateTaskRequest so task-building is asserted without a live gRPC client. */
private class FakeSubmitter(private val fail: Boolean = false) : Submitter {
    var last: CreateTaskRequest? = null
    var closed = false
    override fun createTask(request: CreateTaskRequest): Task {
        if (fail) throw RuntimeException("boom")
        last = request
        return request.task
    }
    override fun close() {
        closed = true
    }
}

private fun env(payload: ByteArray = "hi".toByteArray()) =
    Envelope.new(Kind.LINT, "webhook:/lint", payload, Instant.EPOCH)

class CloudTasksTest : BehaviorSpec({
    Given("a Cloud Tasks transport") {
        When("enqueueing an envelope") {
            Then("it builds a POST task to /internal/dispatch carrying the encoded envelope and Bearer") {
                val fake = FakeSubmitter()
                val t = CloudTasks(fake, QUEUE, URL, "tok", 30.minutes)
                t.enqueue(env())
                val req = requireNotNull(fake.last)
                req.parent shouldBe QUEUE
                val http = req.task.httpRequest
                http.httpMethod shouldBe HttpMethod.POST
                http.url shouldBe URL
                http.headersMap["Content-Type"] shouldBe "application/json"
                http.headersMap["Authorization"] shouldBe "Bearer tok"
                http.body.toStringUtf8() shouldBe String(encode(env()))
                req.task.dispatchDeadline.seconds shouldBe 1_800L
            }
        }

        When("enqueueing with no token") {
            Then("no Authorization header is attached") {
                val fake = FakeSubmitter()
                CloudTasks(fake, QUEUE, URL, "", 30.minutes).enqueue(env())
                requireNotNull(fake.last).task.httpRequest.headersMap.containsKey("Authorization") shouldBe false
            }
        }

        When("enqueueing with a dedup name and a delay") {
            Then("the task name and schedule time are set") {
                val fake = FakeSubmitter()
                val t = CloudTasks(fake, QUEUE, URL, "tok", 30.minutes, now = { Instant.ofEpochSecond(1_000) })
                t.enqueue(env(), EnqueueOptions(name = "lint-acme", delay = 60.seconds))
                val task = requireNotNull(fake.last).task
                task.name shouldBe "$QUEUE/tasks/lint-acme"
                task.scheduleTime.seconds shouldBe 1_060L
            }
        }

        When("the envelope exceeds the Cloud Tasks size limit") {
            Then("it is rejected before any create call") {
                val fake = FakeSubmitter()
                val t = CloudTasks(fake, QUEUE, URL, "tok", 30.minutes)
                shouldThrow<IllegalArgumentException> { t.enqueue(env(ByteArray(MAX_TASK_BYTES))) }
                fake.last shouldBe null
            }
        }

        When("the create call fails") {
            Then("it surfaces a transient error the caller can retry") {
                val t = CloudTasks(FakeSubmitter(fail = true), QUEUE, URL, "tok", 30.minutes)
                shouldThrow<RuntimeException> { t.enqueue(env()) }
            }
        }

        When("closing it") {
            Then("the underlying client is released") {
                val fake = FakeSubmitter()
                CloudTasks(fake, QUEUE, URL, "tok", 30.minutes).close()
                fake.closed shouldBe true
            }
        }
    }
})
