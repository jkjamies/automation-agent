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
    REVIEW = "review"  # GitHub pull_request -> PR code-review agent

    def valid(self) -> bool:
        """Report whether this is a recognized ingest kind."""
        return self in (
            Kind.CRON_DAILY,
            Kind.LINT,
            Kind.COVERAGE,
            Kind.CI,
            Kind.REVIEW,
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
    dispatch), so an absent (or JSON ``null``) value defaults to the zero value — but a
    present value of the wrong type is a malformed body, not a silent default, mirroring Go's
    ``json.Unmarshal`` into the typed ``wireEnvelope`` struct (a non-string ``source`` or a
    non-string/unparseable ``received_at`` is a type error there, i.e. poison).

    Raises:
        ValueError: if the body is not valid JSON, the kind is unknown, the payload is not
            valid standard base64, or ``source``/``received_at`` is present with the wrong
            type (or ``received_at`` is not a parseable RFC 3339 string).
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
        raise ValueError(
            f"ingest: decode payload: want a base64 string, got {type(payload_raw).__name__}"
        )
    try:
        payload = base64.b64decode(payload_raw, validate=True)
    except (ValueError, binascii.Error) as exc:
        raise ValueError(f"ingest: decode payload: {exc}") from exc
    source = _wire_string(wire, "source")
    if "received_at" not in wire or wire["received_at"] is None:
        # Absent or JSON null -> the zero value (Go: time.Time's zero / its UnmarshalJSON
        # treating null as a no-op), never poison.
        received_at = datetime.fromtimestamp(0, tz=UTC)
    else:
        received_raw = wire["received_at"]
        # A present non-string is a malformed body, not a server error (Go: a type error from
        # json.Unmarshal into the time.Time field).
        if not isinstance(received_raw, str):
            raise ValueError(
                "ingest: decode received_at: want an RFC 3339 string, "
                f"got {type(received_raw).__name__}"
            )
        # A present-but-unparseable timestamp (including "") is poison, mirroring Go's
        # time.Parse rejecting a bad RFC 3339 value.
        try:
            received_at = datetime.fromisoformat(received_raw)
        except ValueError as exc:
            raise ValueError(f"ingest: decode received_at: {exc}") from exc
    return new(kind, source, payload, received_at)


def _wire_string(wire: dict, key: str) -> str:
    """Return ``wire[key]`` as a string, mirroring Go's ``json.Unmarshal`` into a typed
    string field: an absent key or JSON ``null`` yields the zero value ``""``, while a
    present non-string value is a malformed body (poison), not a silent default."""
    if key not in wire:
        return ""
    value = wire[key]
    if value is None:  # JSON null unmarshals to the zero value in Go, not an error
        return ""
    if not isinstance(value, str):
        raise ValueError(f"ingest: decode {key}: want a string, got {type(value).__name__}")
    return value
