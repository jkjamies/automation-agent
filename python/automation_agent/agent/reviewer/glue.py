"""The deterministic verify-gate and cross-lens merge logic the glue/synthesis pass owns.

The glue *agent* itself (architectural alignment, testability, and test-coverage reasoning) is
wired in ``agents_setup`` and run in ``review``; cross-lens dedup and the confidence gate are
done here in code rather than asked of the model, so they are deterministic and unit-testable.
"""

from __future__ import annotations

from automation_agent.agent.reviewer.findings import Finding, Severity, severity_rank


def drop_low_confidence(findings: list[Finding], minimum: float) -> list[Finding]:
    """Remove findings below the configured minimum confidence (the phase-1 verify gate). A
    non-positive minimum keeps everything. Never aliases the caller's list."""
    if minimum <= 0:
        return list(findings)
    return [f for f in findings if f.confidence >= minimum]


def dedupe(findings: list[Finding]) -> list[Finding]:
    """Collapse findings that share a fingerprint (same file+line+message, across lenses),
    keeping the one with the worst severity (ties broken by higher confidence). Input order is
    otherwise preserved."""
    seen: dict[str, int] = {}  # fingerprint -> index in out
    out: list[Finding] = []
    for f in findings:
        fp = f.fingerprint()
        i = seen.get(fp)
        if i is not None:
            if _better(f, out[i]):
                out[i] = f
            continue
        seen[fp] = len(out)
        out.append(f)
    return out


def _better(a: Finding, b: Finding) -> bool:
    """Report whether ``a`` should replace ``b`` among duplicates: worse severity wins; on a tie,
    higher confidence."""
    ra, rb = severity_rank(a.severity), severity_rank(b.severity)
    if ra != rb:
        return ra > rb
    return a.confidence > b.confidence


def demote_to_nitpick(findings: list[Finding]) -> list[Finding]:
    """Force every finding to nitpick severity. The catch-all "(other)" category is intentionally
    low-signal, so its findings are demoted rather than allowed to drive the scorecard."""
    for f in findings:
        f.severity = Severity.NITPICK
    return findings
