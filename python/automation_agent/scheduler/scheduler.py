"""Turn cron schedules into ingest envelopes.

Each fire emits a normalized :class:`~automation_agent.ingest.Envelope` so the
root agent treats time-based triggers exactly like any other ingress.
Deterministic tooling — no agent imports.
"""

from __future__ import annotations

from collections.abc import Callable
from datetime import UTC, datetime

from apscheduler.schedulers.background import BackgroundScheduler
from apscheduler.triggers.cron import CronTrigger

from automation_agent.ingest import Envelope, Kind, new

# EmitFunc receives an envelope when a schedule fires.
EmitFunc = Callable[[Envelope], None]

# Cron schedules are interpreted in UTC so "0 9 * * *" means 09:00 UTC regardless of the
# host timezone (which would otherwise be an undocumented, deploy-dependent zone).
_CRON_TZ = UTC


class Scheduler:
    """Registers cron specs that emit ingest envelopes."""

    def __init__(
        self,
        emit: EmitFunc,
        *,
        now: Callable[[], datetime] | None = None,
    ) -> None:
        self.emit = emit
        self.now = now if now is not None else (lambda: datetime.now(UTC))
        self._cron = BackgroundScheduler(timezone=_CRON_TZ)

    def add(self, spec: str, kind: Kind) -> None:
        """Register a 5-field cron spec (minute hour dom month dow).

        Raises for an invalid spec.
        """
        trigger = CronTrigger.from_crontab(spec, timezone=_CRON_TZ)
        self._cron.add_job(self._trigger, trigger=trigger, args=[kind])

    def _trigger(self, kind: Kind) -> None:
        """Emit one envelope; separated from the cron job so it is directly
        unit-testable without waiting for a real schedule."""
        self.emit(new(kind, "scheduler", b"", self.now()))

    def start(self) -> None:
        """Begin the cron loop (non-blocking)."""
        self._cron.start()

    def stop(self) -> None:
        """Halt scheduling."""
        self._cron.shutdown()

    def entries(self) -> int:
        """Report the number of registered schedules."""
        return len(self._cron.get_jobs())
