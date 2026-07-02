"""Deterministic tests for the reviewer engine: the intake decision matrix, the kickoff path,
coalesce-to-latest staleness, the model-calling review pipeline (canned findings), the publish
stage, standards discovery, and the enqueue coalescing hints.

A fake GitHub client captures the writes; a scripted ``FakeLlm`` returns canned JSON. We never
assert on real LLM output — only orchestration and deterministic logic.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

import pytest

from automation_agent.agent import reviewer
from automation_agent.agent.reviewer import standards as standards_mod
from automation_agent.agent.reviewer.enqueue import coalesce_key, enqueue_options
from automation_agent.agent.reviewer.findings import Dimension, Finding, Severity
from automation_agent.agent.reviewer.publish import (
    PublishMeta,
    check_conclusion,
    publish,
    publish_deny,
    sanitize_text,
)
from automation_agent.agent.reviewer.reconcile import fp_marker
from automation_agent.agent.reviewer.reviewer import Engine, split_full_name
from automation_agent.agent.reviewer.scorecard import Level, score_findings
from automation_agent.agent.reviewer.standards import (
    build_standards,
    gate_citations,
    match_standards,
    parse_rules,
    standards_cache_key,
    standards_tools,
)
from automation_agent.githubapi import (
    CheckResult,
    PRFile,
    PullRequestEvent,
    ReviewCommentRef,
    TreeEntry,
)
from automation_agent.ingest import Kind, new
from tests.conftest import FakeLlm


class FakeGH:
    """A stub GitHub client: returns canned files (or raises), and captures the publish writes."""

    def __init__(
        self,
        files: list[PRFile] | None = None,
        *,
        raise_list: Exception | None = None,
        write_error: Exception | None = None,
        minimize_error: Exception | None = None,
        existing: list[ReviewCommentRef] | None = None,
        agent_check: CheckResult | None = None,
        head_sha: str = "",
        head_sha_error: Exception | None = None,
        tree: list[TreeEntry] | None = None,
        truncated: bool = False,
        tree_error: Exception | None = None,
        contents: dict[str, str] | None = None,
    ) -> None:
        self.files = files or []
        self._raise_list = raise_list
        self._write_error = write_error
        self._minimize_error = minimize_error
        self.existing = existing or []
        self._agent_check = agent_check or CheckResult(found=False)
        self._head_sha = head_sha
        self._head_sha_error = head_sha_error
        self._tree = tree or []
        self._truncated = truncated
        self._tree_error = tree_error
        self._contents = contents or {}
        self.calls = 0
        self.review = None
        self.upserts: list[tuple[str, str]] = []
        self.checks: list = []
        self.minimized: list[str] = []

    def list_pr_files(self, owner, repo, number):
        self.calls += 1
        if self._raise_list is not None:
            raise self._raise_list
        return self.files

    def create_review(self, owner, repo, number, in_):
        self.review = in_
        if self._write_error is not None:
            raise self._write_error

    def upsert_marker_comment(self, owner, repo, number, marker, body):
        self.upserts.append((marker, body))
        if self._write_error is not None:
            raise self._write_error

    def create_check_run(self, owner, repo, in_):
        self.checks.append(in_)
        if self._write_error is not None:
            raise self._write_error

    def list_review_comments(self, owner, repo, number):
        if self._write_error is not None:
            raise self._write_error
        return self.existing

    def minimize_comment(self, subject_id):
        self.minimized.append(subject_id)
        if self._minimize_error is not None:
            raise self._minimize_error

    def agent_check(self, owner, repo, ref, check_name):
        return self._agent_check

    def pull_request_head_sha(self, owner, repo, number):
        if self._head_sha_error is not None:
            raise self._head_sha_error
        return self._head_sha

    def tree(self, owner, repo, ref):
        if self._tree_error is not None:
            raise self._tree_error
        return self._tree, self._truncated

    def get_file_content(self, owner, repo, path, ref=""):
        if path in self._contents:
            return self._contents[path]
        raise ValueError(f"fakeGH: no content for {path!r}")


def _engine(gh, *, canned: str = "[]", **overrides) -> Engine:
    llm = FakeLlm(canned)
    deps = reviewer.Deps(
        enabled=True,
        gh=gh,
        base_llm=llm,
        code_llm=llm,
        min_confidence=0.6,
        skip_drafts=True,
        exclude_globs=["go.sum", "vendor/**"],
        max_files=50,
        max_diff_bytes=1000,
    )
    for k, v in overrides.items():
        setattr(deps, k, v)
    return reviewer.new_engine(deps)


def _event(action: str, **kw) -> PullRequestEvent:
    ev = PullRequestEvent(action=action, number=1, repo_full_name="o/r", head_ref="feature/x")
    for k, v in kw.items():
        setattr(ev, k, v)
    return ev


# --- decide matrix -----------------------------------------------------------


def test_decide_matrix() -> None:
    real = [PRFile(path="main.go", patch="abc")]

    gh = FakeGH(real)
    assert _engine(gh).decide(_event("closed")).kind is reviewer.DecisionKind.SKIP
    assert gh.calls == 0  # untriggered action skips before fetch

    gh = FakeGH(real)
    assert _engine(gh).decide(_event("opened", draft=True)).kind is reviewer.DecisionKind.SKIP
    assert gh.calls == 0  # draft skipped pre-fetch

    assert (
        _engine(FakeGH(real)).decide(_event("ready_for_review", draft=True)).kind
        is reviewer.DecisionKind.REVIEW
    )
    assert (
        _engine(FakeGH(real)).decide(_event("opened", head_ref="automation-agent/lint")).kind
        is reviewer.DecisionKind.SKIP
    )
    assert (
        _engine(FakeGH(real)).decide(_event("opened", labels=["skip-review"])).kind
        is reviewer.DecisionKind.SKIP
    )
    assert (
        _engine(FakeGH(real)).decide(_event("opened", author_login="dependabot[bot]")).kind
        is reviewer.DecisionKind.SKIP
    )

    # all-excluded -> skip after fetch
    gh = FakeGH([PRFile(path="go.sum", patch="x"), PRFile(path="vendor/y.go", patch="x")])
    d = _engine(gh).decide(_event("opened"))
    assert d.kind is reviewer.DecisionKind.SKIP and gh.calls == 1

    # normal PR reviews on the filtered size (go.sum excluded)
    gh = FakeGH([PRFile(path="main.go", patch="12345"), PRFile(path="go.sum", patch="ignored")])
    d = _engine(gh).decide(_event("synchronize"))
    assert d.kind is reviewer.DecisionKind.REVIEW and len(d.files) == 1 and d.diff_bytes == 5

    # oversize -> deny
    gh = FakeGH([PRFile(path="a.go", patch="x"), PRFile(path="b.go", patch="x")])
    d = _engine(gh, max_files=1).decide(_event("opened"))
    assert d.kind is reviewer.DecisionKind.DENY and d.reason


def test_decide_malformed_name_and_list_error() -> None:
    with pytest.raises(ValueError):
        _engine(FakeGH([PRFile(path="main.go", patch="x")])).decide(
            _event("opened", repo_full_name="noslash")
        )
    with pytest.raises(ValueError):
        _engine(FakeGH(raise_list=RuntimeError("boom"))).decide(_event("opened"))


def test_split_full_name() -> None:
    assert split_full_name("o/r") == ("o", "r", True)
    for bad in ("noslash", "a/b/c", "/r", "o/"):
        assert split_full_name(bad)[2] is False


# --- kickoff -----------------------------------------------------------------


async def test_kickoff_disabled_noop() -> None:
    gh = FakeGH()
    e = reviewer.new_engine(reviewer.Deps(enabled=False, gh=gh))
    await e.kickoff(b"not even json")
    assert gh.calls == 0


async def test_kickoff_enabled_nil_client_errors() -> None:
    e = reviewer.new_engine(reviewer.Deps(enabled=True, gh=None))
    body = b'{"action":"opened","pull_request":{"number":1,"head":{"ref":"x"}},"repository":{"full_name":"o/r"}}'
    with pytest.raises(ValueError):
        await e.kickoff(body)


async def test_kickoff_malformed_body_errors() -> None:
    with pytest.raises(ValueError):
        await _engine(FakeGH()).kickoff(b"{bad")


async def test_kickoff_review_path() -> None:
    canned = '[{"file":"main.go","line":1,"dimension":"performance","severity":"medium","message":"slow","confidence":0.9}]'
    gh = FakeGH([PRFile(path="main.go", patch="@@\n+x", status="modified")])
    e = _engine(gh, canned=canned)
    body = b'{"action":"opened","pull_request":{"number":7,"head":{"ref":"feature/x"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}'
    await e.kickoff(body)
    assert gh.calls == 1 and len(gh.checks) == 1


async def test_kickoff_staleness() -> None:
    def body(sha: str) -> bytes:
        return (
            '{"action":"synchronize","pull_request":{"number":3,"head":{"ref":"x","sha":"'
            + sha
            + '"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}'
        ).encode()

    real = [PRFile(path="main.go", patch="@@ -1 +1 @@\n+x")]

    gh = FakeGH(real, head_sha="newsha")
    await _engine(gh).kickoff(body("oldsha"))
    assert gh.review is None and not gh.checks and not gh.upserts  # stale -> nothing

    gh = FakeGH(real, head_sha="samesha")
    await _engine(gh).kickoff(body("samesha"))
    assert len(gh.checks) == 1 and len(gh.upserts) == 1  # current -> publishes

    gh = FakeGH(real, head_sha="newsha", head_sha_error=RuntimeError("boom"))
    await _engine(gh).kickoff(body("oldsha"))
    assert len(gh.checks) == 1  # lookup error is best-effort, proceeds


# --- review pipeline ---------------------------------------------------------


async def test_review_pipeline_dedup_and_gate() -> None:
    canned = '[{"file":"main.go","line":10,"dimension":"runtime_safety","severity":"major","message":"nil deref","confidence":0.9}]'
    files = [PRFile(path="main.go", patch="@@ -1 +1 @@\n+x", status="modified")]
    from automation_agent.agent.reviewer.review import run_review

    card, _ = await run_review(_engine(FakeGH(), canned=canned), files, None)
    # Every lens + glue returns the same fingerprint -> dedup to one; one runtime_safety major
    # scores yellow.
    assert card.total == 1 and card.overall is Level.YELLOW


async def test_review_pipeline_drops_low_confidence_and_empty() -> None:
    from automation_agent.agent.reviewer.review import run_review

    files = [PRFile(path="main.go", patch="+x")]
    low = '[{"file":"main.go","line":10,"dimension":"security","severity":"critical","message":"x","confidence":0.2}]'
    card, _ = await run_review(_engine(FakeGH(), canned=low), files, None)
    assert card.total == 0 and card.overall is Level.GREEN

    card, _ = await run_review(_engine(FakeGH(), canned="[]"), files, None)
    assert card.total == 0 and card.overall is Level.GREEN


def test_format_diff() -> None:
    from automation_agent.agent.reviewer.review import format_diff

    out = format_diff(
        [
            PRFile(path="a.go", status="modified", patch="@@ -1 +1 @@\n-old\n+new"),
            PRFile(path="logo.png", status="added", patch=""),
        ]
    )
    assert "### a.go (modified)" in out and "+new" in out
    assert "### logo.png (added)" in out and "(no textual diff available)" in out


# --- publish -----------------------------------------------------------------


def test_publish_routes_findings() -> None:
    files = [PRFile(path="a.go", status="modified", patch="@@ -1,2 +1,3 @@\n a\n+b\n+c\n")]
    findings = [
        Finding(
            file="a.go",
            line=2,
            dimension=Dimension.SECURITY,
            severity=Severity.CRITICAL,
            message="sqli",
            suggestion="safe()",
            fix_prompt="fix it",
        ),
        Finding(
            file="a.go",
            line=99,
            dimension=Dimension.PERFORMANCE,
            severity=Severity.MAJOR,
            message="n+1",
        ),
        Finding(
            file="b.go",
            line=1,
            dimension=Dimension.MAINTAINABILITY,
            severity=Severity.NITPICK,
            message="rename",
        ),
    ]
    gh = FakeGH()
    meta = PublishMeta(owner="o", repo="r", number=7, head_sha="sha1", files=files)
    publish(_engine(gh), score_findings(findings), findings, meta)

    assert gh.review is not None and len(gh.review.comments) == 1
    c = gh.review.comments[0]
    assert c.path == "a.go" and c.line == 2 and c.side == "RIGHT"
    for want in ("🔒 Security", "```suggestion", "Prompt for AI agents"):
        assert want in c.body

    assert len(gh.upserts) == 1
    marker, body = gh.upserts[0]
    assert marker in body
    for want in (
        "automation-agent:review:o/r#7",
        "Agent review",
        "Outside diff range (1)",
        "Nitpicks (1)",
        "a.go:99",
    ):
        assert want in body

    assert len(gh.checks) == 1
    assert gh.checks[0].name == "agent-review" and gh.checks[0].conclusion == "neutral"
    assert gh.checks[0].head_sha == "sha1"


def test_publish_clean_pr_success() -> None:
    gh = FakeGH()
    meta = PublishMeta(owner="o", repo="r", number=1, head_sha="s", files=[PRFile(path="a.go")])
    publish(_engine(gh), score_findings([]), [], meta)
    assert gh.review is None
    assert gh.checks[0].conclusion == "success"
    assert "No findings" in gh.upserts[0][1]


def test_publish_deny() -> None:
    gh = FakeGH()
    meta = PublishMeta(owner="o", repo="r", number=3, head_sha="s")
    publish_deny(_engine(gh), meta, "too big", 200, 999999)
    assert gh.review is None
    assert gh.checks[0].conclusion == "neutral"
    assert "too large" in gh.upserts[0][1]


def test_publish_write_error_propagates() -> None:
    gh = FakeGH(write_error=RuntimeError("boom"))
    meta = PublishMeta(owner="o", repo="r", number=1, head_sha="s")
    with pytest.raises(RuntimeError):
        publish(_engine(gh), score_findings([]), [], meta)


def test_publish_idempotent_on_republished_sha() -> None:
    gh = FakeGH(agent_check=CheckResult(found=True))
    files = [PRFile(path="a.go", status="modified", patch="@@ -1 +1 @@\n+x\n")]
    findings = [
        Finding(
            file="a.go",
            line=1,
            dimension=Dimension.SECURITY,
            severity=Severity.CRITICAL,
            message="x",
        )
    ]
    meta = PublishMeta(owner="o", repo="r", number=1, head_sha="s", files=files)
    publish(_engine(gh), score_findings(findings), findings, meta)
    assert gh.review is None and not gh.upserts and not gh.checks and not gh.minimized


def test_publish_reconciles() -> None:
    files = [PRFile(path="a.go", status="modified", patch="@@ -1 +1 @@\n+x\n")]
    finding = Finding(
        file="a.go",
        line=1,
        dimension=Dimension.SECURITY,
        severity=Severity.CRITICAL,
        message="sqli",
    )
    gh = FakeGH(
        existing=[
            ReviewCommentRef(node_id="keep", body="old " + fp_marker(finding.fingerprint())),
            ReviewCommentRef(node_id="stale", body="fixed " + fp_marker("a.go:9:obsolete")),
            ReviewCommentRef(node_id="foreign", body="human comment no marker"),
        ]
    )
    meta = PublishMeta(owner="o", repo="r", number=1, head_sha="s", files=files)
    publish(_engine(gh), score_findings([finding]), [finding], meta)
    assert gh.review is None  # already present -> not re-posted
    assert gh.minimized == ["stale"]


def test_publish_minimize_failure_still_publishes() -> None:
    files = [PRFile(path="a.go", status="modified", patch="@@ -1 +1 @@\n+x\n")]
    finding = Finding(
        file="a.go", line=1, dimension=Dimension.SECURITY, severity=Severity.CRITICAL, message="new"
    )
    gh = FakeGH(
        minimize_error=RuntimeError("graphql boom"),
        existing=[ReviewCommentRef(node_id="stale", body="fixed " + fp_marker("a.go:9:obsolete"))],
    )
    meta = PublishMeta(owner="o", repo="r", number=1, head_sha="s", files=files)
    publish(_engine(gh), score_findings([finding]), [finding], meta)
    assert gh.minimized == ["stale"] and len(gh.upserts) == 1 and len(gh.checks) == 1


def test_publish_posts_new_finding_with_marker() -> None:
    files = [PRFile(path="a.go", status="modified", patch="@@ -1 +1 @@\n+x\n")]
    finding = Finding(
        file="a.go", line=1, dimension=Dimension.SECURITY, severity=Severity.CRITICAL, message="new"
    )
    gh = FakeGH()
    meta = PublishMeta(owner="o", repo="r", number=1, head_sha="s", files=files)
    publish(_engine(gh), score_findings([finding]), [finding], meta)
    assert gh.review is not None and len(gh.review.comments) == 1
    assert fp_marker(finding.fingerprint()) in gh.review.comments[0].body
    assert not gh.minimized


def test_sanitize_and_check_conclusion() -> None:
    got = sanitize_text("ping @octocat with <b>x</b> & </details>")
    assert "@octocat" not in got and "<b>" not in got
    assert "&lt;b&gt;" in got and "&amp;" in got
    assert check_conclusion(Level.GREEN) == "success"
    assert check_conclusion(Level.YELLOW) == "neutral" and check_conclusion(Level.RED) == "neutral"


# --- standards ---------------------------------------------------------------


def test_match_standards_and_cache_key() -> None:
    entries = [
        TreeEntry(path="AGENTS.md", sha="a", type="blob"),
        TreeEntry(path="internal/AGENTS.md", sha="b", type="blob"),
        TreeEntry(path=".cursor/rules/go.mdc", sha="c", type="blob"),
        TreeEntry(path=".cursor/rules", sha="d", type="tree"),
        TreeEntry(path="main.go", sha="e", type="blob"),
    ]
    got = {
        m.path for m in match_standards(entries, ["AGENTS.md", "**/AGENTS.md", ".cursor/rules/**"])
    }
    assert got == {"AGENTS.md", "internal/AGENTS.md", ".cursor/rules/go.mdc"}

    base = [TreeEntry(path="AGENTS.md", sha="v1")]
    changed = [TreeEntry(path="AGENTS.md", sha="v2")]
    assert standards_cache_key("o", "r", base) != standards_cache_key("o", "r", changed)
    assert standards_cache_key("o", "r", base) == standards_cache_key("o", "r", base)


def test_parse_rules_dedup_blank_and_malformed() -> None:
    raw = (
        "Here are the rules:\n```json\n["
        '{"id":"R1","dimension":"security","summary":"validate input","source":"SECURITY.md"},'
        '{"id":"R1","dimension":"x","summary":"dup id dropped","source":"x"},'
        '{"id":"R2","summary":"  ","source":"y"},'
        '{"id":"R3","dimension":"vibes","summary":"prefer composition","source":"AGENTS.md"}'
        "]\n```"
    )
    got = parse_rules(raw)
    assert len(got) == 2
    assert got[0].id == "R1" and got[0].dimension is Dimension.SECURITY
    assert got[1].id == "R3" and got[1].dimension is Dimension.OTHER
    for bad in ("", "no json", "[{broken", '{"not":"array"}', "[]"):
        assert parse_rules(bad) == []


def test_standards_menu_lookup_and_tools() -> None:
    std = build_standards(
        [reviewer_rule("R1", Dimension.SECURITY, "validate input", "SECURITY.md")],
        {"SECURITY.md": "full security doc text"},
        ["SECURITY.md"],
    )
    assert std is not None and not std.empty()
    assert std.valid_id("R1") and not std.valid_id("R9")
    assert std.rule_doc("R1") == "full security doc text" and std.rule_doc("R9") == ""
    for want in ("R1", "security", "validate input", "SECURITY.md"):
        assert want in std.menu()
    assert len(standards_tools(std)) == 1 and standards_tools(None) == []


def test_gate_citations_modes() -> None:
    std = build_standards(
        [reviewer_rule("R1", Dimension.PATTERN_VIOLATION, "s", "AGENTS.md")],
        {"AGENTS.md": "doc"},
        ["AGENTS.md"],
    )
    findings = [
        Finding(
            dimension=Dimension.PATTERN_VIOLATION,
            severity=Severity.MAJOR,
            message="cited",
            rule_id="R1",
        ),
        Finding(dimension=Dimension.PATTERN_VIOLATION, severity=Severity.MAJOR, message="uncited"),
        Finding(
            dimension=Dimension.ARCHITECTURE,
            severity=Severity.MAJOR,
            message="bad id",
            rule_id="R9",
        ),
        Finding(dimension=Dimension.SECURITY, severity=Severity.CRITICAL, message="sqli"),
    ]
    nitpick = _engine(FakeGH(), standards_enabled=True)
    got = gate_citations(nitpick, list(findings), std)
    assert len(got) == 4
    assert got[0].severity is Severity.MAJOR  # cited untouched
    assert got[1].severity is Severity.NITPICK and got[2].severity is Severity.NITPICK  # demoted
    assert got[3].severity is Severity.CRITICAL  # security never gated

    drop = _engine(FakeGH(), standards_enabled=True, uncited_drop=True)
    assert len(gate_citations(drop, list(findings), std)) == 2

    off = _engine(FakeGH(), standards_enabled=False, uncited_drop=True)
    assert len(gate_citations(off, list(findings), std)) == 4


async def test_discover_standards_variants() -> None:
    rules_json = (
        '[{"id":"R1","dimension":"pattern_violation","summary":"wrap errors","source":"AGENTS.md"}]'
    )

    def engine(gh) -> Engine:
        return _engine(
            gh,
            canned=rules_json,
            standards_enabled=True,
            standards_globs=["AGENTS.md", "**/AGENTS.md", "CONTRIBUTING.md"],
            standards_max_bytes=1 << 20,
        )

    # discovers, distills, caches (second call returns same object)
    gh = FakeGH(
        tree=[TreeEntry(path="AGENTS.md", sha="s1", type="blob")],
        contents={"AGENTS.md": "wrap errors"},
    )
    e = engine(gh)
    std = await standards_mod.discover_standards(e, "o", "r", "head", [])
    assert std is not None and len(std.rules) == 1 and std.source_list() == ["AGENTS.md"]
    again = await standards_mod.discover_standards(e, "o", "r", "head", [])
    assert again is std

    # disabled -> None
    assert (
        await standards_mod.discover_standards(
            _engine(FakeGH(), standards_enabled=False), "o", "r", "head", []
        )
        is None
    )

    # no matching docs -> None
    assert (
        await standards_mod.discover_standards(
            engine(FakeGH(tree=[TreeEntry(path="main.go", sha="s", type="blob")])),
            "o",
            "r",
            "head",
            [],
        )
        is None
    )

    # tree error -> None
    assert (
        await standards_mod.discover_standards(
            engine(FakeGH(tree_error=RuntimeError("boom"))), "o", "r", "head", []
        )
        is None
    )

    # truncated tree -> None
    gh = FakeGH(
        tree=[TreeEntry(path="AGENTS.md", sha="s1", type="blob")],
        truncated=True,
        contents={"AGENTS.md": "x"},
    )
    assert await standards_mod.discover_standards(engine(gh), "o", "r", "head", []) is None

    # partial fetch failure -> None, uncached; then resolves
    gh = FakeGH(
        tree=[
            TreeEntry(path="AGENTS.md", sha="s1", type="blob"),
            TreeEntry(path="CONTRIBUTING.md", sha="s2", type="blob"),
        ],
        contents={"AGENTS.md": "wrap errors"},
    )
    e = engine(gh)
    assert await standards_mod.discover_standards(e, "o", "r", "head", []) is None
    gh._contents["CONTRIBUTING.md"] = "prefer composition"
    std = await standards_mod.discover_standards(e, "o", "r", "head", [])
    assert std is not None and not std.empty()

    # per-module instruction file scoped to touched dirs
    gh = FakeGH(
        tree=[
            TreeEntry(path="AGENTS.md", sha="s0", type="blob"),
            TreeEntry(path="internal/foo/AGENTS.md", sha="s1", type="blob"),
            TreeEntry(path="internal/bar/AGENTS.md", sha="s2", type="blob"),
        ],
        contents={
            "AGENTS.md": "root",
            "internal/foo/AGENTS.md": "foo",
            "internal/bar/AGENTS.md": "bar",
        },
    )
    e = _engine(
        gh,
        canned=rules_json,
        standards_enabled=True,
        standards_globs=["AGENTS.md", "**/AGENTS.md"],
        standards_max_bytes=1 << 20,
    )
    std = await standards_mod.discover_standards(
        e, "o", "r", "head", [PRFile(path="internal/foo/x.go")]
    )
    assert std is not None
    assert set(std.source_list()) == {"AGENTS.md", "internal/foo/AGENTS.md"}


def reviewer_rule(rule_id, dimension, summary, source):
    return standards_mod.Rule(id=rule_id, dimension=dimension, summary=summary, source=source)


# --- enqueue -----------------------------------------------------------------


def _review_envelope(action: str, at: datetime | None = None):
    at = at or datetime.fromtimestamp(1_700_000_000, tz=UTC)
    body = (
        '{"action":"' + action + '","pull_request":{"number":7,"head":{"ref":"x","sha":"s"}},'
        '"repository":{"full_name":"acme/web.api"}}'
    ).encode()
    return new(Kind.REVIEW, "webhook:/github", body, at)


def test_enqueue_synchronize_debounces() -> None:
    opts = enqueue_options(_review_envelope("synchronize"), timedelta(seconds=30))
    assert opts["delay"] == timedelta(seconds=30)
    assert opts["name"].startswith("review-") and "-7-" in opts["name"]


def test_enqueue_buckets_by_window() -> None:
    base = datetime.fromtimestamp(1_700_000_000, tz=UTC)
    a = enqueue_options(
        _review_envelope("synchronize", base + timedelta(seconds=2)), timedelta(seconds=30)
    )
    b = enqueue_options(
        _review_envelope("synchronize", base + timedelta(seconds=5)), timedelta(seconds=30)
    )
    c = enqueue_options(
        _review_envelope("synchronize", base + timedelta(seconds=45)), timedelta(seconds=30)
    )
    assert a["name"] == b["name"]  # same window coalesces
    assert a["name"] != c["name"]  # later window distinct


def test_coalesce_key_lossless_no_collision() -> None:
    a = coalesce_key(PullRequestEvent(repo_full_name="acme/web.api", number=7), 1_700_000_000)
    b = coalesce_key(PullRequestEvent(repo_full_name="acme/web-api", number=7), 1_700_000_000)
    assert a != b


def test_enqueue_immediate_and_disabled() -> None:
    for action in ("opened", "reopened", "ready_for_review"):
        assert enqueue_options(_review_envelope(action), timedelta(seconds=30)) == {}
    assert enqueue_options(_review_envelope("synchronize"), timedelta(0)) == {}
    ci = new(Kind.CI, "webhook:/github", b"{}", datetime.now(UTC))
    assert enqueue_options(ci, timedelta(seconds=30)) == {}
    bad = new(Kind.REVIEW, "webhook:/github", b"{not json", datetime.now(UTC))
    assert enqueue_options(bad, timedelta(seconds=30)) == {}
