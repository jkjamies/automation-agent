"""The normalized event envelope every ingress source is reduced to.

Cloud Scheduler, webhooks, and future hooks (GitHub/Jira/Confluence) are all
normalized to an :class:`Envelope` before being handed to the root agent. See
``.agents/standards/architecture-design.md`` §2.
"""

from __future__ import annotations

import base64
import binascii
import json
from dataclasses import dataclass
from datetime import UTC, datetime
from enum import StrEnum


class Kind(StrEnum):
    """Identifies what triggered an ingest, so the root agent can route it."""

    CRON_DAILY = "cron.daily"  # daily Cloud Scheduler trigger -> summary digest
    LINT = "lint"  # agnostic lint payload -> lint-fixer
    COVERAGE = "coverage"  # agnostic coverage payload -> coverage-fixer
    CI = "ci"  # GitHub check_run -> resume lint/coverage fixer

    def valid(self) -> bool:
        """Report whether this is a recognized ingest kind."""
        return self in (
            Kind.CRON_DAILY,
            Kind.LINT,
            Kind.COVERAGE,
            Kind.CI,
        )


@dataclass
class Envelope:
    """The normalized unit of work.

    ``payload`` carries the raw source body (e.g. the lint JSON or check_run
    event) for the chosen workflow to parse.
    """

    kind: Kind
    source: str  # human-readable origin, e.g. "internal:/cron/daily", "webhook:/lint"
    received_at: datetime
    payload: bytes


def new(kind: Kind, source: str, payload: bytes, at: datetime) -> Envelope:
    """Construct an Envelope."""
    return Envelope(kind=kind, source=source, received_at=at, payload=payload)


def encode(e: Envelope) -> bytes:
    """Serialize an envelope to its JSON wire form for the Cloud Tasks transport (the
    in-process transport passes the object directly and never calls this).

    The wire form is the external contract crossing the task-queue boundary
    (``tasks`` -> POST ``/internal/dispatch``) and must stay byte-identical across all four
    language ports (spec §7): the JSON fields are ``kind`` / ``source`` / ``received_at``
    (RFC 3339) / ``payload``, where ``payload`` is an explicit standard-base64 *string* —
    never a raw byte array — so an empty/absent payload is the empty string in every port,
    with no language-specific null/[]/"" divergence.

    Raises:
        ValueError: if the envelope's kind is not a recognized ingest kind. Rejecting it at
            the enqueue boundary keeps both transports failing the same way — :func:`decode`
            (and POST /internal/dispatch) already drop an unknown kind as a poison task, so
            without this the cloudtasks backend would enqueue successfully and silently
            discard the work later, while inprocess would still hand it to the dispatcher.
    """
    if not e.kind.valid():
        raise ValueError(f"ingest: unknown kind {e.kind!r}")
    # Match the Go reference byte-for-byte: a UTC instant serializes with a trailing "Z"
    # (Go's time.Time RFC 3339), not Python's default "+00:00".
    received_at = e.received_at.isoformat()
    if received_at.endswith("+00:00"):
        received_at = received_at[:-6] + "Z"
    wire = {
        "kind": e.kind.value,
        "source": e.source,
        "received_at": received_at,
        "payload": base64.standard_b64encode(e.payload).decode("ascii"),
    }
    # Compact separators (no spaces) so the bytes match Go's json.Marshal / JS's
    # JSON.stringify output exactly — the wire form is a cross-port external contract.
    return json.dumps(wire, separators=(",", ":")).encode("utf-8")


def decode(b: bytes) -> Envelope:
    """Parse an envelope from its JSON wire form (see :func:`encode`) and reject an unknown
    kind.

    A malformed body, bad base64, or unrecognized kind is a permanent error: the caller
    should ack the delivery rather than retry it — a redelivery cannot fix a poison payload.
    ``source`` and ``received_at`` are informational (only ``kind`` and ``payload`` drive
    dispatch), so they default rather than fail when absent.

    Raises:
        ValueError: if the body is not valid JSON, the kind is unknown, or the payload is
            not valid standard base64.
    """
    try:
        wire = json.loads(b)
    except (ValueError, TypeError) as exc:
        raise ValueError(f"ingest: decode envelope: {exc}") from exc
    if not isinstance(wire, dict):
        raise ValueError(f"ingest: decode envelope: want a JSON object, got {type(wire).__name__}")
    try:
        kind = Kind(wire.get("kind", ""))
    except ValueError as exc:
        raise ValueError(f"ingest: unknown kind {wire.get('kind')!r}") from exc
    if not kind.valid():
        raise ValueError(f"ingest: unknown kind {kind!r}")
    payload_raw = wire.get("payload", "")
    # The wire payload is always a base64 string (Go's typed wireEnvelope). A non-string here
    # is a malformed body, not a server error, so coerce it onto the poison path. Validate
    # strictly (like Go's base64.StdEncoding) so trailing junk is rejected rather than
    # silently discarded.
    if not isinstance(payload_raw, str):
        raise ValueError(f"ingest: decode payload: want a base64 string, got {type(payload_raw).__name__}")
    try:
        payload = base64.b64decode(payload_raw, validate=True)
    except (ValueError, binascii.Error) as exc:
        raise ValueError(f"ingest: decode payload: {exc}") from exc
    received_raw = wire.get("received_at", "")
    if not received_raw:
        received_at = datetime.fromtimestamp(0, tz=UTC)
    else:
        # A present-but-malformed (or non-string) timestamp is poison, mirroring Go's
        # json.Unmarshal rejecting a bad RFC 3339 value.
        try:
            received_at = datetime.fromisoformat(received_raw)
        except (ValueError, TypeError) as exc:
            raise ValueError(f"ingest: decode received_at: {exc}") from exc
    return new(kind, wire.get("source", ""), payload, received_at)
