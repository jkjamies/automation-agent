"""AGENTS.md-presence conformance test.

Every meaningful directory must carry an ``AGENTS.md``. ``specs/`` and hidden
dirs (except ``.agents``) are exempt, as are an agent's content subdirs
(``prompts``/``models``/``tasks``/``testdata``) and Python build artifacts.
"""

from __future__ import annotations

import os


def _skip_doc_dir(base: str) -> bool:
    if base in {
        ".git",
        ".claude",
        "node_modules",
        "vendor",
        "specs",
        ".venv",
        "__pycache__",
        ".pytest_cache",
        ".ruff_cache",
        ".mypy_cache",
        "automation_agent.egg-info",
    }:
        return True
    # Content subdirs of an agent are documented by the agent's shared AGENTS.md.
    if base in {"prompts", "models", "tasks", "testdata"}:
        return True
    # Hidden directories are exempt, except the .agents open-standard dir.
    return base.startswith(".") and base != ".agents"


def test_every_dir_has_agents_doc(archlib) -> None:
    root = archlib.repo_root()
    missing: list[str] = []
    for dirpath, dirnames, _ in os.walk(root):
        dirnames[:] = [d for d in dirnames if not _skip_doc_dir(d)]
        if not os.path.exists(os.path.join(dirpath, "AGENTS.md")):
            r = os.path.relpath(dirpath, root)
            missing.append("(root)" if r == "." else r)
        if os.path.basename(dirpath) == ".agents":
            dirnames[:] = []
    assert not missing, "missing AGENTS.md in: " + ", ".join(missing)
