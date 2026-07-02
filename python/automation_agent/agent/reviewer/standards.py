"""Standards-aware review — steer off the reviewed repo's own conventions.

The reviewer steers off the conventions of the repo *under review* — ``.agents/standards``,
``.cursor/rules``, ``CLAUDE.md``, whatever that repo has, not automation-agent's own. A base-tier
sub-agent distills the discovered docs (heterogeneous formats) into one uniform tagged rule list;
the compact list is injected into every lens and a lazy ``get_rule`` tool serves the full text on
demand. All API-only (no clone).
"""

from __future__ import annotations

import hashlib
import json
import posixpath
import threading
from dataclasses import dataclass, field, replace
from typing import TYPE_CHECKING, cast

from google.adk.tools import BaseTool, FunctionTool

from automation_agent.agent import setup
from automation_agent.agent.reviewer.filter import GlobPattern, glob_to_regexp
from automation_agent.agent.reviewer.findings import (
    Dimension,
    Finding,
    Severity,
    normalize_dimension,
)
from automation_agent.githubapi import PRFile, TreeEntry

if TYPE_CHECKING:
    from automation_agent.agent.reviewer.reviewer import Engine

# The distiller drive kick; the real instruction (the distill prompt + the repo's standards docs)
# lives in the agent's system instruction.
DISTILL_TRIGGER = "Extract the repository's rules as the JSON array specified."


@dataclass(frozen=True)
class Rule:
    """One distilled, dimension-tagged convention rule extracted from the reviewed repo's own
    standards docs."""

    id: str
    dimension: Dimension = Dimension.OTHER
    summary: str = ""
    source: str = ""  # the doc path the rule came from


@dataclass
class Standards:
    """The distilled rule set for one repo at one docs revision: the compact rule menu injected
    into every lens, plus the full source docs for lazy get_rule drill-down."""

    rules: list[Rule] = field(default_factory=list)
    by_id: dict[str, Rule] = field(default_factory=dict)
    docs: dict[str, str] = field(default_factory=dict)  # source path -> full doc text
    sources: list[str] = field(default_factory=list)  # distinct source paths, sorted

    def empty(self) -> bool:
        """Report whether there are no rules to inject, so callers can fall back to generic."""
        return not self.rules

    def menu(self) -> str:
        """Render the compact rule list for an agent prompt: one line per rule (id, dimension,
        summary, source). Small by construction — summaries, not full text."""
        if self.empty():
            return ""
        return "".join(
            f"- {r.id} [{r.dimension.value}] {r.summary} (source: {r.source})\n" for r in self.rules
        )

    def valid_id(self, rule_id: str) -> bool:
        """Report whether ``rule_id`` is a rule in this set (the citation gate's check)."""
        return rule_id in self.by_id

    def rule_doc(self, rule_id: str) -> str:
        """Return the full source-doc text for a rule id, for lazy drill-down. Empty if the id is
        unknown or its source doc is absent."""
        r = self.by_id.get(rule_id)
        if r is None:
            return ""
        return self.docs.get(r.source, "")

    def source_list(self) -> list[str]:
        """Return the applied source paths (empty when no standards), for the summary report."""
        return [] if self.empty() else self.sources


def is_empty(std: Standards | None) -> bool:
    """Report whether a (possibly None) standards set has no rules to inject."""
    return std is None or std.empty()


async def discover_standards(
    engine: Engine, owner: str, repo: str, ref: str, changed: list[PRFile]
) -> Standards | None:
    """Fetch and distill the reviewed repo's convention docs into a tagged rule list, cached per
    repo + docs revision. Returns None (review generic) when standards are disabled, none are
    found, or distillation yields nothing. Best-effort: a discovery/fetch error logs and returns
    None rather than failing the review."""
    if not engine.standards_enabled:
        return None
    try:
        entries, truncated = engine.gh.tree(owner, repo, ref)
    except Exception as exc:  # noqa: BLE001
        engine.log.warning(
            "standards: list tree failed; reviewing generic repo=%s/%s err=%s", owner, repo, exc
        )
        return None
    if truncated:
        # A truncated tree (very large repo) may have missed convention files. Steering off a
        # knowingly-incomplete rule set is worse than a generic review, so degrade to generic
        # (no cache, so a later event with a complete tree retries).
        engine.log.warning(
            "standards: repo tree truncated; reviewing generic repo=%s/%s", owner, repo
        )
        return None
    # Per-module scoping: a per-directory instruction file applies only when the PR touches its
    # module. Repo-global conventions always apply.
    matched = scope_to_touched(match_standards(entries, engine.standards_globs), changed)
    if not matched:
        return None
    # Cache on the matched docs' blob SHAs, so distillation runs once per standards change.
    key = standards_cache_key(owner, repo, matched)
    cached, ok = engine.standards_cache.get(key)
    if ok:
        return cached

    docs: dict[str, str] = {}
    sources: list[str] = []
    total = 0
    fetch_ok = True
    for m in matched:
        try:
            content = engine.gh.get_file_content(owner, repo, m.path, ref)
        except Exception as exc:  # noqa: BLE001
            # A transient fetch failure leaves the rule set incomplete; degrade to generic for
            # this round (and don't memoize, so a later event retries the full set).
            engine.log.warning(
                "standards: fetch failed; reviewing generic path=%s err=%s", m.path, exc
            )
            fetch_ok = False
            break
        if total + len(content) > engine.standards_max_bytes:
            engine.log.warning(
                "standards: byte cap reached; remaining docs skipped cap=%d applied=%d",
                engine.standards_max_bytes,
                len(sources),
            )
            break
        total += len(content)
        docs[m.path] = content
        sources.append(m.path)
    if not fetch_ok or not docs:
        # Incomplete discovery (a fetch failed) or nothing fetched: review generic, uncached.
        return None

    try:
        rules = await distill(engine, docs, sources)
    except Exception as exc:  # noqa: BLE001
        engine.log.warning(
            "standards: distillation failed; reviewing generic repo=%s/%s err=%s", owner, repo, exc
        )
        return None
    std = build_standards(rules, docs, sources)
    # Discovery was complete (whole tree, every matched doc fetched), so memoize — incl. a
    # legitimate empty distill, so a rule-less repo isn't re-distilled until its docs change.
    engine.standards_cache.put(key, std)
    if is_empty(std):
        engine.log.info(
            "standards: discovered docs but distilled no rules; reviewing generic repo=%s/%s docs=%d",
            owner,
            repo,
            len(sources),
        )
        return None
    assert std is not None
    engine.log.info(
        "standards: applied repo=%s/%s rules=%d sources=%s",
        owner,
        repo,
        len(std.rules),
        ", ".join(std.sources),
    )
    return std


def match_standards(entries: list[TreeEntry], globs: list[str]) -> list[TreeEntry]:
    """Return the tree's blob entries whose path matches any standards glob, sorted by path for
    deterministic ordering and cache keys."""
    pats = compile_standards_globs(globs)
    out = [en for en in entries if en.type == "blob" and matches_glob(pats, en.path)]
    out.sort(key=lambda en: en.path)
    return out


def compile_standards_globs(globs: list[str]) -> list[GlobPattern]:
    """Build path matchers from the configured globs. A glob with no '/' matches the basename;
    one with a '/' matches the full path. Reuses the exclude-filter glob compiler."""
    pats: list[GlobPattern] = []
    for g in globs:
        g = g.strip()
        if g == "":
            continue
        pats.append(GlobPattern(re=glob_to_regexp(g), basename="/" not in g))
    return pats


def matches_glob(pats: list[GlobPattern], p: str) -> bool:
    """Report whether ``p`` matches any compiled standards glob."""
    base = posixpath.basename(p)
    for pat in pats:
        target = base if pat.basename else p
        if pat.re.match(target):
            return True
    return False


def scope_to_touched(matched: list[TreeEntry], changed: list[PRFile]) -> list[TreeEntry]:
    """Drop per-directory instruction files (AGENTS.md/CLAUDE.md/GEMINI.md nested below the repo
    root) for modules the PR does not touch — so a finding in one module isn't judged against
    another module's conventions. Repo-global conventions (root files, dotfolder rule dirs,
    linter configs) always apply."""
    touched = touched_dirs(changed)
    out: list[TreeEntry] = []
    for m in matched:
        if module_scoped(m.path) and posixpath.dirname(m.path) not in touched:
            continue
        out.append(m)
    return out


def module_scoped(p: str) -> bool:
    """Report whether a convention file is a per-directory instruction file below the repo root
    (applies only to its own module). Root files and non-instruction conventions are
    repo-global."""
    if posixpath.dirname(p) in ("", "."):
        return False
    return posixpath.basename(p) in ("AGENTS.md", "CLAUDE.md", "GEMINI.md")


def touched_dirs(changed: list[PRFile]) -> set[str]:
    """The set of every ancestor directory (up to the root ".") of the changed files, so a
    per-module instruction file applies when any file in its subtree changed."""
    dirs: set[str] = set()
    for f in changed:
        d = posixpath.dirname(f.path)
        if d == "":
            d = "."
        while True:
            dirs.add(d)
            if d == ".":
                break
            parent = posixpath.dirname(d)
            d = parent if parent != "" else "."
    return dirs


def standards_cache_key(owner: str, repo: str, matched: list[TreeEntry]) -> str:
    """Hash the repo and the matched docs' (path, blob SHA) pairs, so the cache keys on the
    standards revision: any change to a standards file changes its blob SHA and misses."""
    parts = sorted(f"{m.path}:{m.sha}" for m in matched)
    h = hashlib.sha256((f"{owner}/{repo}\n" + "\n".join(parts)).encode("utf-8"))
    return h.hexdigest()


async def distill(engine: Engine, docs: dict[str, str], sources: list[str]) -> list[Rule]:
    """Run the base-tier distiller sub-agent over the discovered docs, returning the parsed rule
    list. Best-effort: a runner/drive error propagates to the caller (which degrades to generic).
    """
    # Deferred import breaks the standards <-> agents_setup module cycle.
    from automation_agent.agent.reviewer import agents_setup

    agent = agents_setup.build_distiller_agent(engine, docs, sources)
    runner = setup.new_runner("reviewer-distill", agent)
    text = await setup.drive_text(runner, "system", "distill", DISTILL_TRIGGER)
    return parse_rules(text)


def build_distiller_instruction(prompt_body: str, docs: dict[str, str], sources: list[str]) -> str:
    """Compose the distiller's instruction: the distill prompt followed by each discovered
    standards doc, fenced so the doc content (untrusted) can't break the prompt."""
    # Imported here to reuse the diff-fencing helper without a module cycle at import time.
    from automation_agent.agent.reviewer.review import max_backtick_run

    parts = [prompt_body, "\n\n## Repository standards documents\n\n"]
    for src in sources:
        doc = docs[src]
        parts.append(f"### Document: {src}\n\n")
        fence = "`" * (max_backtick_run(doc) + 1)
        if len(fence) < 3:
            fence = "```"
        parts.append(fence + "\n")
        parts.append(doc)
        if not doc.endswith("\n"):
            parts.append("\n")
        parts.append(fence + "\n\n")
    return "".join(parts)


def build_standards(
    rules: list[Rule], docs: dict[str, str], sources: list[str]
) -> Standards | None:
    """Assemble the standards from distilled rules + the fetched docs. None when there are no
    rules (so :func:`is_empty` and a generic fallback hold)."""
    if not rules:
        return None
    by_id = {r.id: r for r in rules}
    return Standards(rules=rules, by_id=by_id, docs=docs, sources=sorted(sources))


def parse_rules(raw: str) -> list[Rule]:
    """Extract the distilled rule list from the base model's output. Defensive by design
    (mirrors ``parse_findings``): it scans for the first JSON array that decodes into the rule
    shape, tolerating fences/prose, and never raises — a garbled distillation degrades to "no
    rules" (a generic review) rather than failing."""
    dec = json.JSONDecoder()
    for i, ch in enumerate(raw):
        if ch != "[":
            continue
        try:
            value, _ = dec.raw_decode(raw, i)
        except ValueError:
            continue
        if not isinstance(value, list) or not value or not _valid_rule_array(value):
            continue
        out: list[Rule] = []
        seen: set[str] = set()
        for w in value:
            rule_id = str(w.get("id", "")).strip()
            summary = str(w.get("summary", "")).strip()
            if rule_id == "" or summary == "" or rule_id in seen:
                continue  # a rule needs a unique id and a summary to be usable
            seen.add(rule_id)
            out.append(
                Rule(
                    id=rule_id,
                    dimension=normalize_dimension(str(w.get("dimension", ""))),
                    summary=summary,
                    source=str(w.get("source", "")).strip(),
                )
            )
        if out:
            return out
    return []


def _valid_rule_array(value: list) -> bool:
    """Report whether every element decodes cleanly into the rule shape: an object whose known
    string fields are strings. A type mismatch fails the whole array so the scan moves on."""
    for el in value:
        if not isinstance(el, dict):
            return False
        for key in ("id", "dimension", "summary", "source"):
            if key in el and not isinstance(el[key], str):
                return False
    return True


def standards_tools(std: Standards | None) -> list[BaseTool]:
    """Return the lazy get_rule drill-down tool bound to this run's rule set, or an empty list
    when there are no standards (the lenses then run without it). The compact rule menu lives in
    the prompt; full text is fetched on demand."""
    if is_empty(std):
        return []
    real = cast("Standards", std)

    def get_rule(id: str) -> dict:
        """Return the full source text of a repo standard rule by its id (e.g. "R3") so you can
        read the exact wording before flagging a conformance issue."""
        try:
            return {"rule": real.rule_doc(id.strip())}
        except Exception as exc:  # noqa: BLE001
            return {"error": str(exc)}

    get_rule.__name__ = "get_rule"
    return [cast("BaseTool", FunctionTool(get_rule))]


# The lenses whose findings assert "this violates the repo's documented standard" — they must
# cite a real injected rule_id. Other dimensions (e.g. security) stand on their own.
CONFORMANCE_DIMENSIONS = frozenset({Dimension.PATTERN_VIOLATION, Dimension.ARCHITECTURE})


def gate_citations(engine: Engine, findings: list[Finding], std: Standards | None) -> list[Finding]:
    """Enforce that a conformance finding (pattern/architecture) is anchored to one of the repo's
    own injected rules: an empty or unknown rule_id is dropped or demoted to nitpick per
    REVIEW_UNCITED_MODE. When standards-awareness is off, findings pass through untouched."""
    if not engine.standards_enabled or is_empty(std):
        return findings
    real = cast("Standards", std)
    out: list[Finding] = []
    for f in findings:
        if f.dimension in CONFORMANCE_DIMENSIONS and not real.valid_id(f.rule_id):
            if engine.uncited_drop:
                continue
            f = replace(f, severity=Severity.NITPICK)  # demote an unanchored "violation"
        out.append(f)
    return out


class StandardsCache:
    """Memoizes distilled rule sets per repo + docs revision (in-memory; a cold start
    re-distills). A cached None means "discovered docs, distilled nothing" and is retained so a
    generic repo isn't re-distilled until its docs change."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._m: dict[str, Standards | None] = {}

    def get(self, key: str) -> tuple[Standards | None, bool]:
        with self._lock:
            if key in self._m:
                return self._m[key], True
            return None, False

    def put(self, key: str, std: Standards | None) -> None:
        with self._lock:
            self._m[key] = std
