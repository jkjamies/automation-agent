"""The finding schema, severity/dimension normalization, fingerprint, and the defensive
``parse_findings``.

A category agent emits a JSON array of findings; local models wrap that JSON in prose or ```
fences and occasionally emit nothing, so parsing is best-effort by design — it pulls the first
decodable JSON array out of the text and treats a malformed body as no findings (empty =
success). The narrow single-lens prompts are themselves the false-positive control.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from enum import StrEnum


class Severity(StrEnum):
    """Ranks a finding's importance. critical/major/medium are actionable (posted inline);
    nitpick is collapsed/low-noise."""

    CRITICAL = "critical"
    MAJOR = "major"
    MEDIUM = "medium"
    NITPICK = "nitpick"


def severity_rank(s: Severity) -> int:
    """Order severities (higher = worse) so dedup can keep the worst of a pair."""
    return {
        Severity.CRITICAL: 4,
        Severity.MAJOR: 3,
        Severity.MEDIUM: 2,
        Severity.NITPICK: 1,
    }.get(s, 0)


def normalize_severity(s: str) -> Severity:
    """Map a model-emitted severity onto a known value, defaulting an unknown or blank value to
    nitpick — the safe, low-noise bucket (a local model is biased toward fewer-but-real)."""
    v = s.strip().lower()
    if v == Severity.CRITICAL:
        return Severity.CRITICAL
    if v == Severity.MAJOR:
        return Severity.MAJOR
    if v == Severity.MEDIUM:
        return Severity.MEDIUM
    return Severity.NITPICK


class Dimension(StrEnum):
    """One of the review lenses. A category agent tags each finding with the dimension it
    belongs to; the scorecard is a per-dimension histogram."""

    RUNTIME_SAFETY = "runtime_safety"
    ERROR_HANDLING = "error_handling"
    SECURITY = "security"
    PERFORMANCE = "performance"
    PATTERN_VIOLATION = "pattern_violation"
    MAINTAINABILITY = "maintainability"
    READABILITY = "readability"
    DOCUMENTATION = "documentation"
    ACCESSIBILITY = "accessibility"
    ARCHITECTURE = "architectural_alignment"
    TESTABILITY = "testability"
    TEST_COVERAGE = "test_coverage"
    OTHER = "other"


_KNOWN_DIMENSIONS = frozenset(d.value for d in Dimension)


def normalize_dimension(s: str) -> Dimension:
    """Map a model-emitted dimension onto a known value (lowercased, spaces and hyphens folded
    to underscores), defaulting an unrecognized one to ``other``."""
    d = s.strip().lower().replace(" ", "_").replace("-", "_")
    if d in _KNOWN_DIMENSIONS:
        return Dimension(d)
    return Dimension.OTHER


# The always-on dimensions where a critical finding caps the overall grade to red regardless of
# the other lenses.
CRITICAL_DIMENSIONS = frozenset({Dimension.SECURITY, Dimension.RUNTIME_SAFETY})


@dataclass
class Finding:
    """One review observation from a category agent or the glue pass."""

    file: str = ""
    line: int = 0
    dimension: Dimension = Dimension.OTHER
    severity: Severity = Severity.NITPICK
    message: str = ""
    suggestion: str = ""  # optional ```suggestion body (a localized in-diff fix)
    fix_prompt: str = ""  # optional "Prompt for AI agents" body (feeds the future fix hand-off)
    rule_id: str = ""  # optional repo-standard rule id this finding cites
    confidence: float = 0.0  # 0..1; below REVIEW_MIN_CONFIDENCE is dropped before scoring

    def fingerprint(self) -> str:
        """Identify a finding across re-reviews for reconciliation and for cross-lens dedup:
        file + line + a normalized message. Dimension is deliberately omitted so the same
        line/message surfaced by two different lenses collapses to one finding."""
        return f"{self.file}:{self.line}:{normalize_message(self.message)}"


def normalize_message(s: str) -> str:
    """Lowercase and collapse internal whitespace so trivially different renderings of the same
    message fingerprint identically."""
    return " ".join(s.lower().split())


_STR_FIELDS = ("file", "dimension", "severity", "message", "suggestion", "fix_prompt", "rule_id")


def parse_findings(raw: str) -> list[Finding]:
    """Extract findings from a category agent's raw output. Best-effort by design: it pulls the
    first JSON array out of the text and tolerates a malformed body by returning no findings
    (empty = success). It never raises, so a garbled response degrades to "no findings for this
    lens" rather than failing the whole review."""
    wires = _decode_first_finding_array(raw)
    if not wires:
        return []
    out: list[Finding] = []
    for w in wires:
        msg = str(w.get("message", "")).strip()
        if msg == "":
            continue  # a finding with no message is unusable
        out.append(
            Finding(
                file=str(w.get("file", "")).strip(),
                line=int(w.get("line", 0)),
                dimension=normalize_dimension(str(w.get("dimension", ""))),
                severity=normalize_severity(str(w.get("severity", ""))),
                message=msg,
                suggestion=str(w.get("suggestion", "")).strip(),
                fix_prompt=str(w.get("fix_prompt", "")).strip(),
                rule_id=str(w.get("rule_id", "")).strip(),
                confidence=clamp_confidence(_as_float(w.get("confidence", 0.0))),
            )
        )
    return out


def _decode_first_finding_array(raw: str) -> list[dict] | None:
    """Scan ``raw`` for the first ``[`` that begins a JSON array decoding cleanly into the
    findings shape, returning its elements. Scanning for a *decodable* array (rather than slicing
    the first ``[`` to the last ``]``) tolerates ``` fences, prose, and stray brackets without
    over-grabbing. A valid but empty array is skipped in case a populated one follows; if none
    decodes, it returns None (best-effort: empty = success). Trailing prose after the array is
    ignored."""
    dec = json.JSONDecoder()
    for i, ch in enumerate(raw):
        if ch != "[":
            continue
        try:
            value, _ = dec.raw_decode(raw, i)
        except ValueError:
            continue
        if isinstance(value, list) and value and _valid_finding_array(value):
            return value
    return None


def _valid_finding_array(value: list) -> bool:
    """Report whether every element decodes cleanly into the findings shape: an object whose
    known string fields are strings and whose ``line`` (if present) is an integer. A type
    mismatch fails the whole array so the scan moves on to the next bracket, mirroring a strict
    typed decode."""
    for el in value:
        if not isinstance(el, dict):
            return False
        for key in _STR_FIELDS:
            if key in el and not isinstance(el[key], str):
                return False
        if "line" in el and not (isinstance(el["line"], int) and not isinstance(el["line"], bool)):
            return False
        if "confidence" in el:
            c = el["confidence"]
            if isinstance(c, bool) or not isinstance(c, (int, float)):
                return False
            # A non-finite number (NaN/Infinity) fails the whole array: a strict typed decode
            # rejects those tokens, so the scan moves on to the next bracket rather than
            # admitting a nonsensical confidence.
            if isinstance(c, float) and not math.isfinite(c):
                return False
    return True


def _as_float(v: object) -> float:
    """Coerce a JSON number to float; anything else is treated as unspecified (0)."""
    if isinstance(v, bool):
        return 0.0
    if isinstance(v, (int, float)):
        return float(v)
    return 0.0


def clamp_threshold(f: float) -> float:
    """Normalize a confidence *threshold* into [0,1]. Unlike :func:`clamp_confidence` (which
    treats 0 as "unspecified"), a 0 threshold is meaningful — it disables the gate (keep all) —
    so NaN and negatives fold to 0 (keep all, the safe default) and values above 1 fold to 1."""
    if not (f >= 0):  # also catches NaN
        return 0.0
    if f > 1:
        return 1.0
    return f


def clamp_confidence(c: float) -> float:
    """Keep confidence in [0,1]. A zero/absent value is treated as 0.5 (unspecified) so a model
    that omits the field is not silently dropped by the gate."""
    if c <= 0:
        return 0.5
    if c > 1:
        return 1.0
    return c


def findings_json(findings: list[Finding]) -> str:
    """Render findings as a compact JSON array for embedding in the glue prompt."""
    return json.dumps(
        [
            {
                "file": f.file,
                "line": f.line,
                "dimension": f.dimension.value,
                "severity": f.severity.value,
                "message": f.message,
                "suggestion": f.suggestion,
                "fix_prompt": f.fix_prompt,
                "rule_id": f.rule_id,
                "confidence": f.confidence,
            }
            for f in findings
        ],
        separators=(",", ":"),
    )
