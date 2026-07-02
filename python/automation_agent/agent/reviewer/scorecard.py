"""The count-based scorecard: a per-dimension severity histogram + overall grade.

Count-based, not a synthetic 0–100 score. The overall grade is the critical-cap (any critical in
an always-on critical dimension → red) combined with the worst dimension level.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import IntEnum

from automation_agent.agent.reviewer.findings import (
    CRITICAL_DIMENSIONS,
    Dimension,
    Finding,
    Severity,
)


class Level(IntEnum):
    """A per-dimension and overall grade. Ordered so "worst level wins"."""

    GREEN = 0
    YELLOW = 1
    RED = 2

    def glyph(self) -> str:
        """Render a level as its scorecard glyph."""
        if self is Level.RED:
            return "🔴"
        if self is Level.YELLOW:
            return "🟡"
        return "🟢"

    def word(self) -> str:
        """The textual grade shown beside the glyph in headers and the check."""
        if self is Level.RED:
            return "Red"
        if self is Level.YELLOW:
            return "Yellow"
        return "Green"


@dataclass
class DimScore:
    """The severity histogram for one dimension plus its derived level."""

    dimension: Dimension
    critical: int = 0
    major: int = 0
    medium: int = 0
    nitpick: int = 0
    level: Level = Level.GREEN


@dataclass
class Scorecard:
    """The count-based review result: a per-dimension histogram and an overall grade."""

    dims: list[DimScore] = field(default_factory=list)  # sorted by dimension for stable rendering
    overall: Level = Level.GREEN
    total: int = 0  # total findings counted (after the confidence gate)


def dim_level(s: DimScore) -> Level:
    """Derive a dimension's level from its severity counts (pilot-tunable thresholds): red on
    any critical or ≥2 major; yellow on any major or ≥3 medium; else green."""
    if s.critical >= 1 or s.major >= 2:
        return Level.RED
    if s.major >= 1 or s.medium >= 3:
        return Level.YELLOW
    return Level.GREEN


def score_findings(findings: list[Finding]) -> Scorecard:
    """Build the scorecard from already-confidence-gated findings: a per-dimension histogram +
    level, then overall = critical-cap (any critical in an always-on critical dimension → red)
    combined with the worst dimension level."""
    by_dim: dict[Dimension, DimScore] = {}
    critical_cap = False
    for f in findings:
        d = by_dim.get(f.dimension)
        if d is None:
            d = DimScore(dimension=f.dimension)
            by_dim[f.dimension] = d
        if f.severity is Severity.CRITICAL:
            d.critical += 1
            if f.dimension in CRITICAL_DIMENSIONS:
                critical_cap = True
        elif f.severity is Severity.MAJOR:
            d.major += 1
        elif f.severity is Severity.MEDIUM:
            d.medium += 1
        else:
            d.nitpick += 1

    card = Scorecard(total=len(findings))
    worst = Level.GREEN
    for d in by_dim.values():
        d.level = dim_level(d)
        if d.level > worst:
            worst = d.level
        card.dims.append(d)
    card.dims.sort(key=lambda d: d.dimension.value)

    card.overall = Level.RED if critical_cap else worst
    return card
