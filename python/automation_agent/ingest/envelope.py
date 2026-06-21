"""The normalized event envelope every ingress source is reduced to.

Cron, webhooks, and future hooks (GitHub/Jira/Confluence) are all normalized to
an :class:`Envelope` before being handed to the root agent. See
``docs/architecture.md`` §2.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from enum import StrEnum


class Kind(StrEnum):
    """Identifies what triggered an ingest, so the root agent can route it."""

    CRON_DAILY = "cron.daily"  # 09:00 daily -> summary digest
    CRON_WEEKLY = "cron.weekly"  # 09:00 Monday
    LINT = "lint"  # agnostic lint payload -> lint-fixer
    COVERAGE = "coverage"  # agnostic coverage payload -> coverage-fixer
    CI = "ci"  # GitHub check_run -> resume lint/coverage fixer

    def valid(self) -> bool:
        """Report whether this is a recognized ingest kind."""
        return self in (
            Kind.CRON_DAILY,
            Kind.CRON_WEEKLY,
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
    source: str  # human-readable origin, e.g. "scheduler", "webhook:/lint"
    received_at: datetime
    payload: bytes


def new(kind: Kind, source: str, payload: bytes, at: datetime) -> Envelope:
    """Construct an Envelope."""
    return Envelope(kind=kind, source=source, received_at=at, payload=payload)
