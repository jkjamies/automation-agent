"""Tests for ingest envelope parsing and the wire codec."""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from automation_agent.ingest import Kind, decode, encode, new


def test_kind_valid() -> None:
    for k in (Kind.CRON_DAILY, Kind.LINT, Kind.COVERAGE, Kind.CI):
        assert k.valid()
    assert not Kind.__members__.get("JIRA")  # no such member


def test_new() -> None:
    at = datetime.fromtimestamp(1718870400, tz=UTC)
    e = new(Kind.LINT, "webhook:/lint", b'{"x":1}', at)
    assert e.kind == Kind.LINT
    assert e.source == "webhook:/lint"
    assert e.payload == b'{"x":1}'
    assert e.received_at == at


# The wire codec round-trips every field, including a payload that is not valid UTF-8 (it
# travels as base64, so arbitrary bytes survive) and an empty payload.
@pytest.mark.parametrize(
    "payload",
    [
        b'{"action":"completed"}',  # json
        bytes([0x00, 0xFF, 0xFE, 0x10, 0x80]),  # binary
        b"",  # empty
    ],
)
def test_encode_decode_round_trip(payload: bytes) -> None:
    at = datetime.fromtimestamp(1718870400, tz=UTC)
    e = new(Kind.CI, "webhook:/github", payload, at)
    out = decode(encode(e))
    assert out.kind == e.kind
    assert out.source == e.source
    assert out.received_at == e.received_at
    assert out.payload == payload


def test_encode_wire_shape() -> None:
    # The wire form is byte-identical to the Go reference: compact separators (no spaces), a
    # UTC instant spelled with a trailing "Z", and a standard-base64 payload ("hi" -> "aGk=").
    b = encode(new(Kind.LINT, "webhook:/lint", b"hi", datetime.fromtimestamp(0, tz=UTC)))
    text = b.decode()
    assert text == '{"kind":"lint","source":"webhook:/lint","received_at":"1970-01-01T00:00:00Z","payload":"aGk="}'


def test_decode_rejects_bad_input() -> None:
    # A malformed body, an unknown kind, an undecodable payload, and a non-string payload are
    # all permanent (poison) errors.
    with pytest.raises(ValueError):
        decode(b"not json")
    with pytest.raises(ValueError):
        decode(b'{"kind":"jira","source":"x"}')
    with pytest.raises(ValueError):
        decode(b'{"kind":"ci","source":"x","payload":"@@@not-base64"}')
    # Strict base64: valid alphabet but with trailing junk is rejected (lenient decoding would
    # silently drop it), matching Go's base64.StdEncoding.
    with pytest.raises(ValueError):
        decode(b'{"kind":"ci","source":"x","payload":"aGk=junk"}')
    # A non-string payload is a malformed body, not a server error — still poison.
    with pytest.raises(ValueError):
        decode(b'{"kind":"ci","source":"x","payload":123}')
