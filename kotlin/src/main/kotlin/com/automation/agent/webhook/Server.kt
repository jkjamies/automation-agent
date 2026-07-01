/*
 * Package webhook exposes the HTTP ingress endpoints. Each request is reduced to a
 * normalized ingest.Envelope and handed to an IngestFunc, which should enqueue and return
 * quickly. Deterministic tooling — no agent imports.
 */
package com.automation.agent.webhook

import com.automation.agent.ingest.Envelope
import com.automation.agent.ingest.Kind
import com.automation.agent.ingest.decode
import io.ktor.http.HttpStatusCode
import io.ktor.server.application.Application
import io.ktor.server.application.ApplicationCall
import io.ktor.server.cio.CIO
import io.ktor.server.engine.EmbeddedServer
import io.ktor.server.engine.EngineConnectorBuilder
import io.ktor.server.engine.embeddedServer
import io.ktor.server.request.header
import io.ktor.server.request.receiveChannel
import io.ktor.server.response.respond
import io.ktor.server.response.respondText
import io.ktor.server.routing.RoutingContext
import io.ktor.server.routing.get
import io.ktor.server.routing.post
import io.ktor.server.routing.routing
import io.ktor.utils.io.readRemaining
import kotlinx.io.readByteArray
import java.lang.System.Logger.Level
import java.time.Instant

/** The largest request body accepted on any webhook endpoint (matches the Go reference's 5 MiB). */
private const val MAX_BODY_BYTES = 5 * 1024 * 1024

/** Idle connections are closed after this many seconds, blunting slow-client (Slowloris) holds. */
private const val CONNECTION_IDLE_TIMEOUT_SECONDS = 30

/** An empty request body, used for the parameterless internal cron/sweep triggers. */
private val EMPTY_BODY = ByteArray(0)

/** The default logger for the dispatch worker when none is injected (the nil-logger guard). */
private val DEFAULT_LOG: System.Logger = System.getLogger("automation-agent.webhook")

/**
 * Consumes a normalized envelope. It should enqueue work and return quickly; a thrown
 * exception becomes a 500 to the caller.
 */
fun interface IngestFunc {
    suspend operator fun invoke(envelope: Envelope)
}

/**
 * Runs an envelope's workflow synchronously, in-request. It backs POST /internal/dispatch, which the
 * Cloud Tasks transport delivers to so the compute runs while CPU is allocated. Typically the root
 * dispatcher's dispatch. A thrown exception is a transient failure (retried by the queue).
 */
fun interface DispatchFunc {
    suspend operator fun invoke(envelope: Envelope)
}

/** Resolves every engine's parked runs whose CI never reported (the periodic timeout backstop). */
fun interface SweepFunc {
    suspend operator fun invoke()
}

/**
 * Installs the webhook routes on an [Application]. [secret] enables HMAC verification of all
 * webhook POST routes (empty = skip, local dev only); [now] is injectable for deterministic
 * `receivedAt` timestamps in tests.
 */
fun Application.webhookRoutes(
    ingest: IngestFunc,
    secret: String = "",
    now: () -> Instant = Instant::now,
    internalToken: String = "",
    sweep: SweepFunc? = null,
    dispatch: DispatchFunc? = null,
    log: System.Logger = DEFAULT_LOG,
) {
    routing {
        get("/healthz") { call.respondText("ok") }

        post("/webhooks/lint") {
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            if (!call.authenticated(secret, body)) return@post call.respond(HttpStatusCode.Unauthorized, "invalid signature")
            accept(ingest, Envelope.new(Kind.LINT, "webhook:/lint", body, now()))
        }

        post("/webhooks/coverage") {
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            if (!call.authenticated(secret, body)) return@post call.respond(HttpStatusCode.Unauthorized, "invalid signature")
            accept(ingest, Envelope.new(Kind.COVERAGE, "webhook:/coverage", body, now()))
        }

        post("/webhooks/github") {
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            if (!call.authenticated(secret, body)) return@post call.respond(HttpStatusCode.Unauthorized, "invalid signature")
            accept(ingest, Envelope.new(Kind.CI, "webhook:/github", body, now()))
        }

        // Internal Cloud Scheduler ingress: the daily digest + the durable timeout sweep. Guarded by
        // a Bearer INTERNAL_TOKEN; an unset token disables the routes entirely (404).
        post("/internal/cron/daily") {
            if (!call.internalAuthorized(internalToken)) return@post
            accept(ingest, Envelope.new(Kind.CRON_DAILY, "internal:/cron/daily", EMPTY_BODY, now()))
        }

        post("/internal/sweep") {
            if (!call.internalAuthorized(internalToken)) return@post
            if (sweep == null) return@post call.respond(HttpStatusCode.NotImplemented, "sweep not configured")
            try {
                sweep()
                call.respond(HttpStatusCode.OK, "ok")
            } catch (_: Exception) {
                call.respond(HttpStatusCode.InternalServerError, "sweep failed")
            }
        }

        // The Cloud Tasks worker: runs one queued envelope's workflow synchronously in-request so
        // Cloud Run keeps CPU allocated for the whole compute (unlike a post-202 background task).
        // Retry classification follows Cloud Tasks' retry-on-non-2xx contract (spec §6): a transient
        // dispatch failure -> 500 so the queue retries with backoff; a poison (undecodable) body ->
        // 200 + log so the queue drops it rather than looping. The same INTERNAL_TOKEN Bearer guards
        // it; an unwired dispatcher -> 501.
        post("/internal/dispatch") {
            if (!call.internalAuthorized(internalToken)) return@post
            if (dispatch == null) return@post call.respond(HttpStatusCode.NotImplemented, "dispatch not configured")
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            val envelope = try {
                decode(body)
            } catch (e: IllegalArgumentException) {
                // Permanent: ack so Cloud Tasks does not redeliver a poison payload. decode signals
                // every poison case (unknown kind, bad base64, malformed body) as IllegalArgumentException,
                // so catching only that acks genuine poison while letting an unexpected bug surface as a
                // 500 (retried) rather than being silently swallowed as a dropped task.
                log.log(Level.WARNING, "dropping undecodable dispatch task", e)
                return@post call.respond(HttpStatusCode.OK, "ok")
            }
            try {
                dispatch(envelope)
                call.respond(HttpStatusCode.OK, "ok")
            } catch (e: Exception) {
                // Transient: let Cloud Tasks retry with backoff.
                log.log(Level.ERROR, "dispatch failed kind=${envelope.kind} source=${envelope.source}", e)
                call.respond(HttpStatusCode.InternalServerError, "dispatch failed")
            }
        }
    }
}

/**
 * Reads the request body, bounded at [MAX_BODY_BYTES]. Returns null when the body exceeds the cap,
 * so the caller can reply 413 — unlike the Go reference's `io.LimitReader`, an oversized body is
 * rejected outright rather than silently truncated (which would only fail HMAC later). Memory is
 * bounded: at most `MAX_BODY_BYTES + 1` bytes are ever read.
 */
/**
 * Verifies the request's HMAC signature when [secret] is set. With no secret (local dev only)
 * every request passes. A kickoff selects the caller-supplied target repo, so the lint/coverage
 * routes are authenticated with the same secret as the GitHub webhook.
 */
private fun ApplicationCall.authenticated(secret: String, body: ByteArray): Boolean {
    if (secret.isEmpty()) return true
    val sig = request.header("X-Hub-Signature-256") ?: ""
    return verifySignature(secret, sig, body)
}

/**
 * Authorizes an internal request: an unset [token] disables these routes (responds 404); otherwise
 * the request must carry a matching `Bearer <token>` (constant-time compared, 401 on mismatch).
 * Returns false (after responding) when the request must not proceed.
 */
private suspend fun ApplicationCall.internalAuthorized(token: String): Boolean {
    if (token.isEmpty()) {
        respond(HttpStatusCode.NotFound, "not found")
        return false
    }
    val provided = request.header("Authorization") ?: ""
    if (!constantTimeEquals(provided, "Bearer $token")) {
        respond(HttpStatusCode.Unauthorized, "unauthorized")
        return false
    }
    return true
}

private fun constantTimeEquals(a: String, b: String): Boolean =
    java.security.MessageDigest.isEqual(a.toByteArray(Charsets.UTF_8), b.toByteArray(Charsets.UTF_8))

private suspend fun ApplicationCall.receiveCapped(): ByteArray? {
    val bytes = receiveChannel().readRemaining((MAX_BODY_BYTES + 1).toLong()).readByteArray()
    return if (bytes.size > MAX_BODY_BYTES) null else bytes
}

private suspend fun RoutingContext.accept(ingest: IngestFunc, e: Envelope) {
    try {
        ingest(e)
        call.respond(HttpStatusCode.Accepted)
    } catch (_: Exception) {
        call.respond(HttpStatusCode.InternalServerError, "ingest failed")
    }
}

/**
 * Builds an embedded Ktor (CIO) server serving the webhook routes on [port]. Composition for
 * the entrypoint; call `.start(wait = …)` on the result.
 */
fun webhookServer(
    port: Int,
    ingest: IngestFunc,
    secret: String = "",
    now: () -> Instant = Instant::now,
    internalToken: String = "",
    sweep: SweepFunc? = null,
    dispatch: DispatchFunc? = null,
    log: System.Logger = DEFAULT_LOG,
    // An entrypoint hook applied to the [Application] before the routes are installed, e.g. to add
    // the tracing interceptor. Kept as an injected block so this package stays free of that concern.
    configure: Application.() -> Unit = {},
): EmbeddedServer<*, *> {
    val bindPort = port
    return embeddedServer(
        CIO,
        configure = {
            connectors.add(EngineConnectorBuilder().apply { this.port = bindPort })
            connectionIdleTimeoutSeconds = CONNECTION_IDLE_TIMEOUT_SECONDS
        },
        module = {
            configure()
            webhookRoutes(ingest, secret, now, internalToken, sweep, dispatch, log)
        },
    )
}
