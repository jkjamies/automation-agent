/*
 * Package webhook exposes the HTTP ingress endpoints. Each request is reduced to a
 * normalized ingest.Envelope and handed to an IngestFunc, which should enqueue and return
 * quickly. Deterministic tooling — no agent imports.
 */
package io.github.jkjamies.automationagent.webhook

import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind
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
import java.time.Instant

/** The largest request body accepted on any webhook endpoint (matches the Go reference's 5 MiB). */
private const val MAX_BODY_BYTES = 5 * 1024 * 1024

/** Idle connections are closed after this many seconds, blunting slow-client (Slowloris) holds. */
private const val CONNECTION_IDLE_TIMEOUT_SECONDS = 30

/**
 * Consumes a normalized envelope. It should enqueue work and return quickly; a thrown
 * exception becomes a 500 to the caller.
 */
fun interface IngestFunc {
    suspend operator fun invoke(envelope: Envelope)
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
) {
    routing {
        get("/healthz") { call.respondText("ok") }

        post("/webhooks/lint") {
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            if (!call.authenticated(secret, body)) return@post call.respond(HttpStatusCode.Unauthorized, "invalid signature")
            dispatch(ingest, Envelope.new(Kind.LINT, "webhook:/lint", body, now()))
        }

        post("/webhooks/coverage") {
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            if (!call.authenticated(secret, body)) return@post call.respond(HttpStatusCode.Unauthorized, "invalid signature")
            dispatch(ingest, Envelope.new(Kind.COVERAGE, "webhook:/coverage", body, now()))
        }

        post("/webhooks/github") {
            val body = call.receiveCapped() ?: return@post call.respond(HttpStatusCode.PayloadTooLarge, "payload too large")
            if (!call.authenticated(secret, body)) return@post call.respond(HttpStatusCode.Unauthorized, "invalid signature")
            dispatch(ingest, Envelope.new(Kind.CI, "webhook:/github", body, now()))
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

private suspend fun ApplicationCall.receiveCapped(): ByteArray? {
    val bytes = receiveChannel().readRemaining((MAX_BODY_BYTES + 1).toLong()).readByteArray()
    return if (bytes.size > MAX_BODY_BYTES) null else bytes
}

private suspend fun RoutingContext.dispatch(ingest: IngestFunc, e: Envelope) {
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
): EmbeddedServer<*, *> {
    val bindPort = port
    return embeddedServer(
        CIO,
        configure = {
            connectors.add(EngineConnectorBuilder().apply { this.port = bindPort })
            connectionIdleTimeoutSeconds = CONNECTION_IDLE_TIMEOUT_SECONDS
        },
        module = { webhookRoutes(ingest, secret, now) },
    )
}
