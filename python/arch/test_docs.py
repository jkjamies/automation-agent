"""OKF bundle conformance tests.

The system's knowledge lives in the repo-root ``okf/`` bundle (Open Knowledge Format).
These tests are the bundle's structural gate: every concept opens with YAML frontmatter
declaring a non-empty ``type``, every directory carries an ``index.md``, every
bundle-absolute link resolves, and the repo-root ``AGENTS.md`` still points at the
bundle index. Structure only, never content.
"""

from __future__ import annotations

import os
import re

_TYPE_LINE = re.compile(r"^type:\s*\S", re.MULTILINE)
_ABS_LINK = re.compile(r"\]\((/[^)#]+\.md)(?:#[^)]*)?\)")
_RESERVED = {"index.md", "log.md", "AGENTS.md"}


def _okf_root(archlib) -> str:
    root = os.path.normpath(os.path.join(archlib.repo_root(), "..", "okf"))
    assert os.path.isdir(root), f"okf bundle missing at {root}"
    return root


def _md_files(root: str):
    for dirpath, _, filenames in os.walk(root):
        for name in filenames:
            if name.endswith(".md"):
                yield os.path.join(dirpath, name), name


def test_okf_concepts_have_frontmatter_type(archlib) -> None:
    bad: list[str] = []
    for path, name in _md_files(_okf_root(archlib)):
        if name in _RESERVED:
            continue
        with open(path, encoding="utf-8") as f:
            body = f.read()
        if not body.startswith("---\n"):
            bad.append(f"{path}: missing frontmatter block")
            continue
        end = body.find("\n---", 4)
        if end < 0:
            bad.append(f"{path}: frontmatter block not closed")
            continue
        if not _TYPE_LINE.search(body[4:end]):
            bad.append(f"{path}: frontmatter missing required non-empty type field")
    assert bad == []


def test_okf_every_dir_has_index(archlib) -> None:
    missing: list[str] = []
    for dirpath, _, filenames in os.walk(_okf_root(archlib)):
        if "index.md" not in filenames:
            missing.append(dirpath)
    assert missing == []


def test_okf_bundle_links_resolve(archlib) -> None:
    """Bundle-absolute links (with or without a #fragment) resolve to files in the
    bundle. Anchor existence inside the target is content, not structure, and is
    deliberately not validated."""
    root = _okf_root(archlib)
    dangling: list[str] = []
    for path, _ in _md_files(root):
        with open(path, encoding="utf-8") as f:
            body = f.read()
        for link in _ABS_LINK.findall(body):
            if not os.path.isfile(os.path.join(root, link.lstrip("/"))):
                dangling.append(f"{path}: {link}")
    assert dangling == []


def test_okf_root_agents_doc_points_at_bundle(archlib) -> None:
    p = os.path.normpath(os.path.join(archlib.repo_root(), "..", "AGENTS.md"))
    with open(p, encoding="utf-8") as f:
        assert "okf/index.md" in f.read()
