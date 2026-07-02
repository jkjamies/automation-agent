"""The debounce/coalesce transport hints for a synchronize review.

Rapid pushes to one PR are collapsed so only the latest SHA is reviewed: a ``synchronize`` review
is enqueued with a debounce delay under a per-PR-per-window Cloud Tasks dedup name, so a burst of
pushes collapses to one delayed task. ``opened``/``reopened``/``ready_for_review`` enqueue
immediately (a human is waiting on the first review). Coalescing is a workflow concern, so it
lives here rather than in the transport (which stays dumb about PRs and SHAs).
"""

from __future__ import annotations

import base64
from datetime import UTC, datetime, timedelta
from typing import TypedDict

from automation_agent.githubapi import PullRequestEvent, parse_pull_request_event
from automation_agent.ingest import Envelope, Kind

# Nanoseconds between the proleptic-calendar zero instant (Jan 1, year 1 UTC) and the Unix epoch.
# The debounce window is floored relative to that zero instant, not the Unix epoch, so the bucket
# carried in the dedup name must be computed with the same origin to stay byte-identical across
# every port (the name is a cross-port external contract).
_UNIX_TO_INTERNAL_NS = 62135596800 * 1_000_000_000


class EnqueueOptions(TypedDict, total=False):
    """The transport hints a review envelope carries: the Cloud Tasks dedup ``name`` and the
    debounce ``delay``. ``**``-unpacked into :meth:`Transport.enqueue`; an empty mapping means
    immediate, undeduplicated enqueue."""

    name: str
    delay: timedelta


def enqueue_options(e: Envelope, debounce: timedelta) -> EnqueueOptions:
    """Return the transport hints for a review envelope so rapid pushes coalesce. A
    pull_request "synchronize" (a new push to an open PR) is enqueued under a per-PR dedup name
    with a debounce delay, so a burst of pushes collapses to one delayed task that reviews the
    latest SHA; the worker's staleness check then enforces newest-wins. Any non-review kind, an
    unparseable payload, or a non-positive debounce yields no options (immediate enqueue). Only
    the Cloud Tasks backend honors the hints; the in-process backend ignores them.
    """
    if e.kind is not Kind.REVIEW or debounce <= timedelta(0):
        return {}
    try:
        ev = parse_pull_request_event(e.payload)
    except ValueError:
        return {}
    if ev.action != "synchronize":
        return {}
    bucket = _truncate_to_window(e.received_at, debounce)
    return {"name": coalesce_key(ev, bucket), "delay": debounce}


def coalesce_key(ev: PullRequestEvent, bucket_unix_ns: int) -> str:
    """The per-PR-per-window Cloud Tasks dedup name. Identically-named tasks collapse to one, so a
    burst of pushes within a debounce window coalesces to a single review of the latest SHA.

    The name carries a time bucket (the receipt time floored to the debounce window) because
    Cloud Tasks keeps a task name reserved for ~1h after the task completes or is deleted: a fixed
    per-PR name would make a push that lands minutes after the previous review collide with the
    reserved name and silently drop the new review. Bucketing gives each window a fresh name.

    The repo full name is base64url-encoded so the name is both valid in the Cloud Tasks charset
    ([A-Za-z0-9_-]) and lossless: a naive replace-invalid-with-'-' would collapse distinct repos
    (e.g. "acme/web.api" and "acme/web-api") to the same name and silently drop one PR's review.
    """
    encoded = (
        base64.urlsafe_b64encode(ev.repo_full_name.encode("utf-8")).rstrip(b"=").decode("ascii")
    )
    return f"review-{encoded}-{ev.number}-{bucket_unix_ns}"


def _truncate_to_window(at: datetime, debounce: timedelta) -> int:
    """Floor ``at`` to a multiple of ``debounce`` measured from the proleptic-calendar zero
    instant (see ``_UNIX_TO_INTERNAL_NS``), returning the result as Unix nanoseconds. Computing
    the window origin this way keeps the bucket byte-identical across every port."""
    unix_ns = _unix_nanos(at)
    window_ns = _timedelta_nanos(debounce)
    if window_ns <= 0:
        return unix_ns
    return unix_ns - ((unix_ns + _UNIX_TO_INTERNAL_NS) % window_ns)


def _unix_nanos(at: datetime) -> int:
    """Return ``at`` as integer Unix nanoseconds (a naive datetime is treated as UTC)."""
    if at.tzinfo is None:
        at = at.replace(tzinfo=UTC)
    delta = at - datetime(1970, 1, 1, tzinfo=UTC)
    return (delta.days * 86400 + delta.seconds) * 1_000_000_000 + delta.microseconds * 1000


def _timedelta_nanos(d: timedelta) -> int:
    """Return a timedelta as integer nanoseconds."""
    return (d.days * 86400 + d.seconds) * 1_000_000_000 + d.microseconds * 1000
