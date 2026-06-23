"""HTTP ingress endpoints.

Each request is reduced to a normalized :class:`~automation_agent.ingest.Envelope`
and handed to an ``IngestFunc``, which should enqueue and return quickly.
Deterministic tooling — no agent imports.
"""

from __future__ import annotations

import hashlib
import hmac
from collections.abc import Awaitable, Callable
from datetime import UTC, datetime

from fastapi import FastAPI, Request, Response

from automation_agent.ingest import Envelope, Kind, new

# maxBodyBytes caps how much of a webhook body we read.
MAX_BODY_BYTES = 5 << 20  # 5 MiB


class _BodyTooLarge(Exception):
    """Raised when a request body exceeds MAX_BODY_BYTES (caller returns 413)."""

# IngestFunc consumes a normalized envelope. It should enqueue work and return
# quickly; a raised error becomes a 500 to the caller.
IngestFunc = Callable[[Envelope], Awaitable[None]]


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
        now: Callable[[], datetime] | None = None,
    ) -> None:
        self.ingest = ingest
        self.secret = secret
        self.now = now if now is not None else (lambda: datetime.now(UTC))
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
            return await self._dispatch(
                new(Kind.LINT, "webhook:/lint", body, self.now())
            )

        @app.post("/webhooks/coverage")
        async def coverage(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._take_body(request)
            if isinstance(body, Response):
                return body
            if not self._authenticated(request, body):
                return Response(content="invalid signature", status_code=401)
            return await self._dispatch(
                new(Kind.COVERAGE, "webhook:/coverage", body, self.now())
            )

        @app.post("/webhooks/github")
        async def github(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._take_body(request)
            if isinstance(body, Response):
                return body
            if not self._authenticated(request, body):
                return Response(content="invalid signature", status_code=401)
            return await self._dispatch(
                new(Kind.CI, "webhook:/github", body, self.now())
            )

        return app

    def _authenticated(self, request: Request, body: bytes) -> bool:
        """Verify the request's HMAC signature when a secret is configured. With no secret
        (local dev only) every request passes. A kickoff selects the caller-supplied target
        repo, so the lint/coverage routes are authenticated with the same secret as the
        GitHub webhook."""
        if self.secret == "":
            return True
        return verify_signature(
            self.secret, request.headers.get("X-Hub-Signature-256", ""), body
        )

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
