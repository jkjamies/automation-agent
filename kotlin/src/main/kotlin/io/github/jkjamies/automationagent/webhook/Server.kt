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
import io.ktor.server.cio.CIO
import io.ktor.server.engine.embeddedServer
import io.ktor.server.request.header
import io.ktor.server.request.receive
import io.ktor.server.response.respond
import io.ktor.server.response.respondText
import io.ktor.server.routing.RoutingContext
import io.ktor.server.routing.get
import io.ktor.server.routing.post
import io.ktor.server.routing.routing
import java.time.Instant

/**
 * Consumes a normalized envelope. It should enqueue work and return quickly; a thrown
 * exception becomes a 500 to the caller.
 */
fun interface IngestFunc {
    suspend operator fun invoke(envelope: Envelope)
}

/**
 * Installs the webhook routes on an [Application]. [secret] enables HMAC verification of
 * /webhooks/github (empty = skip, local dev only); [now] is injectable for deterministic
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
            dispatch(ingest, Envelope.new(Kind.LINT, "webhook:/lint", call.receive<ByteArray>(), now()))
        }

        post("/webhooks/coverage") {
            dispatch(ingest, Envelope.new(Kind.COVERAGE, "webhook:/coverage", call.receive<ByteArray>(), now()))
        }

        post("/webhooks/github") {
            val body = call.receive<ByteArray>()
            val sig = call.request.header("X-Hub-Signature-256") ?: ""
            if (secret.isNotEmpty() && !verifySignature(secret, sig, body)) {
                call.respond(HttpStatusCode.Unauthorized, "invalid signature")
                return@post
            }
            dispatch(ingest, Envelope.new(Kind.CI, "webhook:/github", body, now()))
        }
    }
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
) = embeddedServer(CIO, port = port) { webhookRoutes(ingest, secret, now) }
