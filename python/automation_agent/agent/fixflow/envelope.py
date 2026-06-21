"""The kickoff envelope a CI job posts.

Port of ``fixflow/envelope.go``. ``repo``/``base`` identify where to work (trusted);
``report`` is arbitrary tool output (lint report, coverage report, …) the triage LLM
reasons over. The report is kept as raw JSON text so :meth:`Kickoff.report_text`
mirrors Go's ``json.RawMessage`` semantics: a JSON string is unquoted, any other JSON
value is passed through verbatim.
"""

from __future__ import annotations

import json
from dataclasses import dataclass


def _split_repo(s: str) -> tuple[str, str, bool]:
    owner, sep, repo = s.partition("/")
    if not sep or owner == "" or repo == "":
        return "", "", False
    return owner, repo, True


@dataclass
class Kickoff:
    """The trusted envelope a CI job posts. ``report`` is the raw JSON text of the
    report value (mirrors Go's ``json.RawMessage``)."""

    repo: str
    report: str
    base: str = "main"

    def validate(self) -> None:
        """Check the trusted fields; the report is intentionally not parsed."""
        if self.repo.strip() == "":
            raise ValueError("kickoff: repo is required")
        if not _split_repo(self.repo)[2]:
            raise ValueError(f"kickoff: repo {self.repo!r} must be owner/name")
        if not self.report or self.report.strip() == "":
            raise ValueError("kickoff: report is required")

    def report_text(self) -> str:
        """Return the report as clean text for the LLM. A JSON-string report (wrapping
        text/XML like lcov or JaCoCo) is unquoted; any other JSON value is passed
        through as-is."""
        s = self.report.strip()
        if s.startswith('"'):
            try:
                unquoted = json.loads(self.report)
                if isinstance(unquoted, str):
                    return unquoted
            except (ValueError, TypeError):
                pass
        return self.report

    def owner(self) -> str:
        return _split_repo(self.repo)[0]

    def name(self) -> str:
        return _split_repo(self.repo)[1]


def parse_kickoff(b: bytes) -> Kickoff:
    """Unmarshal and validate the envelope, applying defaults."""
    try:
        raw = json.loads(b)
    except (ValueError, TypeError) as exc:
        raise ValueError(f"parse kickoff: {exc}") from exc
    if not isinstance(raw, dict):
        raise ValueError("parse kickoff: expected a JSON object")

    repo = raw.get("repo")
    repo = repo if isinstance(repo, str) else ""
    base = raw.get("base")
    base = base if isinstance(base, str) and base != "" else "main"

    # Preserve the report's raw JSON text so report_text() can mirror RawMessage.
    if "report" in raw and raw["report"] is not None:
        report = json.dumps(raw["report"], separators=(",", ":"))
    else:
        report = ""

    k = Kickoff(repo=repo, report=report, base=base)
    k.validate()
    return k
