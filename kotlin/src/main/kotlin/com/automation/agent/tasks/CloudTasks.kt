package com.automation.agent.tasks

import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.encode
import com.google.cloud.tasks.v2.CloudTasksClient
import com.google.cloud.tasks.v2.CreateTaskRequest
import com.google.cloud.tasks.v2.HttpMethod
import com.google.cloud.tasks.v2.HttpRequest
import com.google.cloud.tasks.v2.QueueName
import com.google.cloud.tasks.v2.Task
import com.google.protobuf.ByteString
import com.google.protobuf.Timestamp
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.time.Instant
import kotlin.coroutines.cancellation.CancellationException
import kotlin.time.Duration
import kotlin.time.toJavaDuration
import com.google.protobuf.Duration as ProtoDuration

/**
 * MAX_TASK_BYTES is the Cloud Tasks size limit for an HTTP-target task (1 MiB; verify against
 * current quota docs). enqueue refuses an envelope whose encoded body exceeds it rather than letting
 * Cloud Tasks reject the create call opaquely (spec §9). Today's payloads are metadata well under
 * this (PR diffs are fetched later via the API, not carried in the webhook body); if a future
 * payload could exceed it, the fallback is store-in-Firestore + enqueue a reference — noted in the
 * spec, not built here.
 */
const val MAX_TASK_BYTES = 1 shl 20

/**
 * The slice of the Cloud Tasks client this backend uses, isolated so the task-building can be
 * unit-tested against a fake without a live gRPC connection.
 */
interface Submitter {
    fun createTask(request: CreateTaskRequest): Task
    fun close()
}

/**
 * Enqueues each envelope as a Cloud Tasks HTTP-target task pointed at /internal/dispatch — the
 * production backend. The queue gives durable retry with backoff (a task survives the instance being
 * reclaimed mid-run and is redelivered) and rate limiting (the queue's max-concurrent-dispatches
 * replaces the in-process semaphore), and the worker runs in-request so CPU stays allocated for the
 * whole compute.
 */
class CloudTasks(
    private val client: Submitter,
    private val queuePath: String,
    private val dispatchUrl: String,
    private val token: String,
    // Explicit per-task dispatch deadline. The HTTP-target default is only 10m, so a longer workflow
    // would be cancelled mid-run and retried (duplicating side effects). Zero leaves it unset so the
    // queue default applies — production always supplies it via config.
    private val deadline: Duration,
    private val now: () -> Instant = Instant::now,
) : Transport {

    /**
     * Builds and submits a task carrying the JSON-encoded envelope as its body and the
     * INTERNAL_TOKEN as a Bearer header. [EnqueueOptions.name] sets the task name (Cloud Tasks
     * dedup); [EnqueueOptions.delay] sets the schedule time.
     *
     * @throws IllegalArgumentException if the encoded body exceeds the Cloud Tasks task-size limit.
     * @throws RuntimeException if the create call fails (a transient failure the caller surfaces as
     *   a 500 so the queue retries).
     */
    override suspend fun enqueue(e: Envelope, opts: EnqueueOptions) {
        val body = encode(e)
        require(body.size <= MAX_TASK_BYTES) {
            "tasks: envelope is ${body.size} bytes, over the $MAX_TASK_BYTES-byte Cloud Tasks task limit"
        }

        val http = HttpRequest.newBuilder()
            .setHttpMethod(HttpMethod.POST)
            .setUrl(dispatchUrl)
            .putHeaders("Content-Type", "application/json")
            .setBody(ByteString.copyFrom(body))
        if (token.isNotEmpty()) {
            http.putHeaders("Authorization", "Bearer $token")
        }

        val task = Task.newBuilder().setHttpRequest(http.build())
        if (deadline > Duration.ZERO) {
            val d = deadline.toJavaDuration()
            task.dispatchDeadline = ProtoDuration.newBuilder().setSeconds(d.seconds).setNanos(d.nano).build()
        }
        opts.name?.let { task.name = "$queuePath/tasks/$it" }
        if (opts.delay > Duration.ZERO) {
            val at = now().plus(opts.delay.toJavaDuration())
            task.scheduleTime = Timestamp.newBuilder().setSeconds(at.epochSecond).setNanos(at.nano).build()
        }

        val request = CreateTaskRequest.newBuilder().setParent(queuePath).setTask(task.build()).build()
        try {
            withContext(Dispatchers.IO) { client.createTask(request) }
        } catch (ce: CancellationException) {
            throw ce
        } catch (ex: Exception) {
            throw RuntimeException("tasks: create task: ${ex.message}", ex)
        }
    }

    /** Releases the underlying Cloud Tasks client. */
    override suspend fun close() {
        withContext(Dispatchers.IO) { client.close() }
    }
}

/**
 * Opens a Cloud Tasks client and targets the queue
 * projects/<project>/locations/<location>/queues/<queue>. [dispatchUrl] is the full URL of the
 * /internal/dispatch worker; [token] is the static INTERNAL_TOKEN the task carries as a Bearer
 * header (the same auth that endpoint already enforces). [deadline] is the explicit per-task
 * dispatch deadline (config validated to Cloud Tasks' 15s..30m range). [CloudTasks.close] releases
 * the client.
 */
fun newCloudTasks(
    project: String,
    location: String,
    queue: String,
    dispatchUrl: String,
    token: String,
    deadline: Duration,
): CloudTasks {
    val client = CloudTasksClient.create()
    val submitter = object : Submitter {
        override fun createTask(request: CreateTaskRequest): Task = client.createTask(request)
        override fun close() = client.close()
    }
    return CloudTasks(submitter, QueueName.of(project, location, queue).toString(), dispatchUrl, token, deadline)
}
