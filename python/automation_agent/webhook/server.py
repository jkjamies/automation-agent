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
        """Return the FastAPI app to mount (the ``Handler()`` analogue)."""
        return self._app

    def _build_app(self) -> FastAPI:
        app = FastAPI()

        @app.get("/healthz")
        async def healthz() -> Response:  # pyright: ignore[reportUnusedFunction]
            return Response(content="ok", media_type="text/plain")

        @app.post("/webhooks/lint")
        async def lint(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._read_body(request)
            if body is None:
                return Response(content="read body", status_code=400)
            return await self._dispatch(
                new(Kind.LINT, "webhook:/lint", body, self.now())
            )

        @app.post("/webhooks/coverage")
        async def coverage(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._read_body(request)
            if body is None:
                return Response(content="read body", status_code=400)
            return await self._dispatch(
                new(Kind.COVERAGE, "webhook:/coverage", body, self.now())
            )

        @app.post("/webhooks/github")
        async def github(request: Request) -> Response:  # pyright: ignore[reportUnusedFunction]
            body = await self._read_body(request)
            if body is None:
                return Response(content="read body", status_code=400)
            if self.secret != "" and not verify_signature(
                self.secret,
                request.headers.get("X-Hub-Signature-256", ""),
                body,
            ):
                return Response(content="invalid signature", status_code=401)
            return await self._dispatch(
                new(Kind.CI, "webhook:/github", body, self.now())
            )

        return app

    async def _read_body(self, request: Request) -> bytes | None:
        """Read up to MAX_BODY_BYTES, truncating anything larger (mirrors Go's
        ``io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))``: oversize bodies are
        capped, not rejected). Streams so an oversize body never buffers in full.
        Returns None only on a transport read error."""
        chunks: list[bytes] = []
        total = 0
        try:
            async for chunk in request.stream():
                chunks.append(chunk)
                total += len(chunk)
                if total >= MAX_BODY_BYTES:
                    break
        except Exception:
            return None
        return b"".join(chunks)[:MAX_BODY_BYTES]

    async def _dispatch(self, env: Envelope) -> Response:
        try:
            await self.ingest(env)
        except Exception:
            return Response(content="ingest failed", status_code=500)
        return Response(status_code=202)
