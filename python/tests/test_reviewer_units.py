"""Deterministic unit tests for the reviewer's pure modules — findings parsing, filtering, the
size gate, category selection, the scorecard, and the verify/dedup gates. No assertions on LLM
output; canned strings only."""

from __future__ import annotations

from automation_agent.agent.reviewer.categories import (
    Tier,
    has_ui_files,
    select_categories,
)
from automation_agent.agent.reviewer.filter import FileFilter, glob_to_regexp, patch_bytes
from automation_agent.agent.reviewer.findings import (
    Dimension,
    Finding,
    Severity,
    clamp_confidence,
    clamp_threshold,
    normalize_dimension,
    normalize_severity,
    parse_findings,
)
from automation_agent.agent.reviewer.glue import dedupe, demote_to_nitpick, drop_low_confidence
from automation_agent.agent.reviewer.scorecard import Level, score_findings
from automation_agent.agent.reviewer.sizegate import oversize
from automation_agent.githubapi import PRFile

# --- findings ----------------------------------------------------------------


def test_parse_findings_extracts_first_array_tolerating_prose_and_fences() -> None:
    raw = (
        "Here are the findings:\n```json\n"
        '[{"file":"a.go","line":2,"dimension":"Security","severity":"CRITICAL",'
        '"message":" sqli ","confidence":0.9}]\n```\ntrailing prose'
    )
    got = parse_findings(raw)
    assert len(got) == 1
    f = got[0]
    assert f.file == "a.go" and f.line == 2
    assert f.dimension is Dimension.SECURITY and f.severity is Severity.CRITICAL
    assert f.message == "sqli" and f.confidence == 0.9


def test_parse_findings_skips_empty_array_then_takes_populated() -> None:
    raw = '[]\n[{"file":"a","line":1,"message":"m"}]'
    got = parse_findings(raw)
    assert len(got) == 1 and got[0].message == "m"


def test_parse_findings_drops_messageless_and_malformed() -> None:
    assert parse_findings('[{"file":"a","line":1,"message":"  "}]') == []
    for raw in ("", "no json here", "[{broken", '{"not":"array"}', "[]"):
        assert parse_findings(raw) == []


def test_parse_findings_wrong_typed_line_falls_through() -> None:
    # A line that is not an integer fails the strict shape check for that array; the scan then
    # finds no other decodable array and yields nothing.
    assert parse_findings('[{"file":"a","line":"x","message":"m"}]') == []


def test_parse_findings_rejects_non_finite_confidence() -> None:
    # NaN/Infinity confidence fails the whole array (a strict typed decode rejects those tokens),
    # so the scan moves on and yields nothing.
    for tok in ("NaN", "Infinity", "-Infinity"):
        assert parse_findings(f'[{{"file":"a","line":1,"message":"m","confidence":{tok}}}]') == []


def test_normalize_helpers() -> None:
    assert normalize_severity("MAJOR") is Severity.MAJOR
    assert normalize_severity("bogus") is Severity.NITPICK
    assert normalize_dimension("Runtime-Safety") is Dimension.RUNTIME_SAFETY
    assert normalize_dimension("vibes") is Dimension.OTHER
    assert (
        clamp_confidence(0) == 0.5 and clamp_confidence(2) == 1.0 and clamp_confidence(0.3) == 0.3
    )
    assert clamp_threshold(-1) == 0.0 and clamp_threshold(5) == 1.0 and clamp_threshold(0.4) == 0.4


def test_fingerprint_omits_dimension_and_normalizes_message() -> None:
    a = Finding(file="a.go", line=3, dimension=Dimension.SECURITY, message="Hello   World")
    b = Finding(file="a.go", line=3, dimension=Dimension.PERFORMANCE, message="hello world")
    assert a.fingerprint() == b.fingerprint() == "a.go:3:hello world"


# --- filter ------------------------------------------------------------------


def test_glob_to_regexp_semantics() -> None:
    assert glob_to_regexp("*.min.js").match("app.min.js")
    assert not glob_to_regexp("*.min.js").match("dir/app.min.js")  # * does not cross /
    assert glob_to_regexp("vendor/**").match("vendor/a/b.go")  # ** crosses /
    assert glob_to_regexp("go.sum").match("go.sum")


def test_filter_apply_keeps_and_sizes_filtered_set() -> None:
    flt = FileFilter(["go.sum", "vendor/**", " "])
    files = [
        PRFile(path="main.go", patch="12345"),
        PRFile(path="go.sum", patch="ignored"),
        PRFile(path="vendor/x.go", patch="ignored"),
    ]
    kept, diff_bytes = flt.apply(files)
    assert [f.path for f in kept] == ["main.go"]
    assert diff_bytes == 5


def test_patch_bytes_charges_omitted_diff_from_line_counts() -> None:
    assert patch_bytes(PRFile(path="a", patch="abcd")) == 4
    assert patch_bytes(PRFile(path="a", additions=2, deletions=1)) == 3 * 50  # omitted text diff
    assert patch_bytes(PRFile(path="logo.png")) == 0  # binary


# --- sizegate ----------------------------------------------------------------


def test_oversize_two_dimensional_and_disabled() -> None:
    reason, denied = oversize(3, 10, 1, 0)
    assert denied and "3 changed files" in reason
    reason, denied = oversize(1, 999, 50, 100)
    assert denied and "999 diff bytes" in reason
    assert oversize(1, 1, 0, 0) == ("", False)  # both dimensions disabled


# --- categories --------------------------------------------------------------


def test_select_categories_gates_accessibility_on_ui() -> None:
    non_ui = [PRFile(path="main.go")]
    names = {c.name for c in select_categories(non_ui)}
    assert "accessibility" not in names and "safety" in names
    ui = [PRFile(path="app.tsx")]
    assert "accessibility" in {c.name for c in select_categories(ui)}
    assert has_ui_files(ui) and not has_ui_files(non_ui)


def test_category_tiers() -> None:
    by = {c.name: c for c in select_categories([PRFile(path="app.tsx")])}
    assert by["security"].tier is Tier.CODE and by["performance"].tier is Tier.BASE
    assert by["other"].other and by["accessibility"].ui_only


# --- scorecard ---------------------------------------------------------------


def test_score_findings_levels_and_critical_cap() -> None:
    # A single critical in a critical dimension caps overall to red.
    card = score_findings(
        [Finding(dimension=Dimension.SECURITY, severity=Severity.CRITICAL, message="m")]
    )
    assert card.overall is Level.RED and card.total == 1
    # Two majors in a non-critical dimension -> red dimension -> red overall (no cap needed).
    card = score_findings(
        [
            Finding(dimension=Dimension.PERFORMANCE, severity=Severity.MAJOR, message=str(i))
            for i in range(2)
        ]
    )
    assert card.overall is Level.RED
    # One major -> yellow.
    card = score_findings(
        [Finding(dimension=Dimension.PERFORMANCE, severity=Severity.MAJOR, message="m")]
    )
    assert card.overall is Level.YELLOW
    # Only nitpicks -> green.
    assert score_findings([Finding(severity=Severity.NITPICK, message="m")]).overall is Level.GREEN
    assert score_findings([]).overall is Level.GREEN


# --- glue --------------------------------------------------------------------


def test_drop_low_confidence() -> None:
    fs = [Finding(message="a", confidence=0.9), Finding(message="b", confidence=0.2)]
    assert [f.message for f in drop_low_confidence(fs, 0.6)] == ["a"]
    assert drop_low_confidence(fs, 0) == fs  # non-positive keeps all


def test_dedupe_keeps_worst_severity() -> None:
    a = Finding(file="a", line=1, severity=Severity.MEDIUM, message="m", confidence=0.5)
    b = Finding(file="a", line=1, severity=Severity.CRITICAL, message="m", confidence=0.5)
    out = dedupe([a, b])
    assert len(out) == 1 and out[0].severity is Severity.CRITICAL


def test_demote_to_nitpick() -> None:
    fs = [Finding(severity=Severity.CRITICAL, message="m")]
    assert demote_to_nitpick(fs)[0].severity is Severity.NITPICK
