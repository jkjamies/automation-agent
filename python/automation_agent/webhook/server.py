"""HTTP ingress endpoints.

Each request is reduced to a normalized :class:`~automation_agent.ingest.Envelope`
and handed to an ``IngestFunc``, which should enqueue and return quickly.
Deterministic tooling — no agent imports.
"""

from __future__ import annotations

import hashlib
import hmac
import logging
from collections.abc import Awaitable, Callable
from datetime import UTC, datetime

from fastapi import FastAPI, Request, Response

from automation_agent.ingest import Envelope, Kind, decode, new

# maxBodyBytes caps how much of a webhook body we read.
MAX_BODY_BYTES = 5 << 20  # 5 MiB


class _BodyTooLarge(Exception):
    """Raised when a request body exceeds MAX_BODY_BYTES (caller returns 413)."""


# IngestFunc consumes a normalized envelope. It should enqueue work and return
# quickly; a raised error becomes a 500 to the caller.
IngestFunc = Callable[[Envelope], Awaitable[None]]

# SweepFunc resolves parked runs whose CI never reported (the durable timeout catch-all).
# Driven by Cloud Scheduler via POST /internal/sweep.
SweepFunc = Callable[[], Awaitable[None]]

# DispatchFunc runs an envelope's workflow synchronously, in-request. It backs
# POST /internal/dispatch, which the Cloud Tasks transport delivers to so the compute runs
# on allocated CPU (unlike a post-202 background task). It is the root dispatcher's dispatch.
DispatchFunc = Callable[[Envelope], Awaitable[None]]


def verify_signature(secret: str, header: str, body: bytes) -> bool:
    """Check a GitHub ``sha256=<hex>`` HMAC over the request body."""
    prefix = "sha256="
    if not header.startswith(prefix):
        return False
    mac = hmac.new(secret.encode(), body, hashlib.sha256)
    want = mac.hexdigest()
    return hmac.compare_digest(want, header[len(prefix) :])


class Server:
    """Routes webhook requests to an IngestFunc."""

    def __init__(
        self,
        ingest: IngestFunc,
        *,
        secret: str = "",
        internal_token: str = "",
        sweep: SweepFunc | None = None,
        dispatch: DispatchFunc | None = None,
        now: Callable[[], datetime] | None = None,
        log: logging.Logger | None = None,
    ) -> None:
        self.ingest = ingest
        self.secret = secret
        # internal_token guards the /internal/* endpoints (Cloud Scheduler cron + sweep, and
        # the Cloud Tasks dispatch worker). Empty disables them (404), so they are never open
        # by default.
        self.internal_token = internal_token
        self.sweep_fn = sweep
        # dispatch_fn runs a queued envelope in-request (the Cloud Tasks worker endpoint).
        # When unset, POST /internal/dispatch returns 501.
        self.dispatch_fn = dispatch
        self.now = now if now is not None else (lambda: datetime.now(UTC))
        # Logger for non-fatal handler diagnostics (e.g. a poison /internal/dispatch body
        # that is acked rather than retried).
        self.log = log if log is not None else logging.getLogger("automation_agent")
        self._app = self._build_app()

    @property
    def app(self) -> FastAPI:
        """Return the FastAPI app to mount."""
        return self._app

    def _build_app(self) -> FastAPI:
        app = FastAPI()

        @app.get("/healthz")
        async def healthz() -> Response:  # pyright: ignore[reportUnusedFunction]
            return Response(content="ok", media_type="text/plain")

        @app.post("/webhooks/lint")
        async def lint(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._take_body(request)
            if isinstance(body, Response):
                return body
            if not self._authenticated(request, body):
                return Response(content="invalid signature", status_code=401)
            return await self._dispatch(new(Kind.LINT, "webhook:/lint", body, self.now()))

        @app.post("/webhooks/coverage")
        async def coverage(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._take_body(request)
            if isinstance(body, Response):
                return body
            if not self._authenticated(request, body):
                return Response(content="invalid signature", status_code=401)
            return await self._dispatch(new(Kind.COVERAGE, "webhook:/coverage", body, self.now()))

        # The single GitHub App webhook door. The App delivers every event to this one URL,
        # so it routes by the X-GitHub-Event header:
        #   - pull_request -> KindReview: kick off the PR code-review agent (native-event
        #     kickoff).
        #   - check_run    -> KindCI:     resume a parked lint/coverage fix run.
        # Any other event (e.g. ping, or one the App is subscribed to but this service
        # ignores) is acknowledged with 200 and not dispatched, so GitHub records a
        # successful delivery. HMAC verification applies to every delivery before routing.
        @app.post("/webhooks/github")
        async def github(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._take_body(request)
            if isinstance(body, Response):
                return body
            if not self._authenticated(request, body):
                return Response(content="invalid signature", status_code=401)
            event = request.headers.get("X-GitHub-Event", "")
            if event == "pull_request":
                return await self._dispatch(new(Kind.REVIEW, "webhook:/github", body, self.now()))
            if event == "check_run":
                return await self._dispatch(new(Kind.CI, "webhook:/github", body, self.now()))
            return Response(status_code=200)

        # Cloud Scheduler ingress (Bearer-token auth; disabled with a 404 unless
        # internal_token is set). Lets the daily schedule live GCP-side so the instance can
        # scale to zero.
        @app.post("/internal/cron/daily")
        async def cron_daily(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            denied = self._internal_authenticated(request)
            if denied is not None:
                return denied
            return await self._dispatch(
                new(Kind.CRON_DAILY, "internal:/cron/daily", b"", self.now())
            )

        @app.post("/internal/sweep")
        async def sweep(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            denied = self._internal_authenticated(request)
            if denied is not None:
                return denied
            if self.sweep_fn is None:
                return Response(content="sweep not configured", status_code=501)
            try:
                await self.sweep_fn()
            except Exception:
                return Response(content="sweep failed", status_code=500)
            return Response(status_code=200)

        # Cloud Tasks worker: executes a queued envelope in-request (same Bearer auth).
        # Running in-request (not in a post-202 background task) keeps CPU allocated on Cloud
        # Run for the whole compute. Retry classification follows Cloud Tasks'
        # retry-on-non-2xx contract (spec §6): a transient failure (the dispatch raises —
        # LLM/network/Firestore) returns 500 so the queue retries with backoff; a permanent
        # failure (a malformed body or unknown kind, which a retry cannot fix) is acked with
        # 200 and logged so the queue drops the poison task instead of looping.
        @app.post("/internal/dispatch")
        async def dispatch(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            denied = self._internal_authenticated(request)
            if denied is not None:
                return denied
            if self.dispatch_fn is None:
                return Response(content="dispatch not configured", status_code=501)
            body = await self._take_body(request)
            if isinstance(body, Response):
                return body
            try:
                env = decode(body)
            except ValueError as exc:
                # Permanent: ack so Cloud Tasks does not redeliver a poison payload.
                self.log.warning("dropping undecodable dispatch task: %s", exc)
                return Response(status_code=200)
            try:
                await self.dispatch_fn(env)
            except Exception as exc:  # noqa: BLE001
                # Transient: let Cloud Tasks retry with backoff.
                self.log.error(
                    "dispatch failed: kind=%s source=%s err=%s", env.kind, env.source, exc
                )
                return Response(content="dispatch failed", status_code=500)
            return Response(status_code=200)

        return app

    def _internal_authenticated(self, request: Request) -> Response | None:
        """Guard the /internal/* endpoints with a Bearer token. Returns the error Response
        to send (404 when no token is configured — disabled by default; 401 on a missing or
        wrong token), or None when the request is authorized."""
        if self.internal_token == "":
            return Response(content="internal endpoints disabled", status_code=404)
        prefix = "Bearer "
        auth = request.headers.get("Authorization", "")
        if not auth.startswith(prefix):
            return Response(content="unauthorized", status_code=401)
        if not hmac.compare_digest(auth[len(prefix) :], self.internal_token):
            return Response(content="unauthorized", status_code=401)
        return None

    def _authenticated(self, request: Request, body: bytes) -> bool:
        """Verify the request's HMAC signature when a secret is configured. With no secret
        (local dev only) every request passes. A kickoff selects the caller-supplied target
        repo, so the lint/coverage routes are authenticated with the same secret as the
        GitHub webhook."""
        if self.secret == "":
            return True
        return verify_signature(self.secret, request.headers.get("X-Hub-Signature-256", ""), body)

    async def _take_body(self, request: Request) -> bytes | Response:
        """Read the request body, or return the error response to send: a transport read
        error -> 400, an oversize body -> 413. Callers ``isinstance``-check the result."""
        try:
            body = await self._read_body(request)
        except _BodyTooLarge:
            return Response(content="payload too large", status_code=413)
        if body is None:
            return Response(content="read body", status_code=400)
        return body

    async def _read_body(self, request: Request) -> bytes | None:
        """Read up to MAX_BODY_BYTES. Streams so an oversize body never buffers in
        full: as soon as the cap is exceeded it raises :class:`_BodyTooLarge` (the
        caller returns 413) rather than truncating — a truncated body would fail
        HMAC and could feed malformed JSON downstream. Returns None only on a
        transport read error."""
        chunks: list[bytes] = []
        total = 0
        too_large = False
        try:
            async for chunk in request.stream():
                total += len(chunk)
                if total > MAX_BODY_BYTES:
                    too_large = True
                    break
                chunks.append(chunk)
        except Exception:
            return None
        if too_large:
            raise _BodyTooLarge
        return b"".join(chunks)

    async def _dispatch(self, env: Envelope) -> Response:
        try:
            await self.ingest(env)
        except Exception:
            return Response(content="ingest failed", status_code=500)
        return Response(status_code=202)
