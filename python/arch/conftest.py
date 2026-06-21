"""Shared helpers for the architecture-conformance tests, exposed as the ``archlib``
fixture (so the tests need no package-relative imports under importlib mode).

These mirror ``ARCH/arch_test.go`` and ``ARCH/docs_test.go`` from the Go variant:
they parse every Python module's imports with the ``ast`` module (the analogue of
Go's ``go/parser`` ImportsOnly walk) and assert the same import boundaries.
"""

from __future__ import annotations

import ast
from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace

import pytest

_SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "specs", ".venv", "__pycache__"}


@dataclass
class FileImports:
    path: Path
    imports: list[str]


def repo_root() -> Path:
    """The Python project root (the parent of the ``arch/`` directory)."""
    return Path(__file__).resolve().parent.parent


def python_files(root: Path) -> list[FileImports]:
    """Parse every ``.py`` file under ``root`` and return its imported module paths."""
    out: list[FileImports] = []
    for p in sorted(root.rglob("*.py")):
        if any(part in _SKIP_DIRS for part in p.parts):
            continue
        tree = ast.parse(p.read_text(), filename=str(p))
        imps: list[str] = []
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                imps.extend(alias.name for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
                # record both the module and each module.symbol, so e.g.
                # `from google.adk.models import Gemini` is detectable as
                # "google.adk.models.Gemini".
                imps.append(node.module)
                imps.extend(f"{node.module}.{alias.name}" for alias in node.names)
        out.append(FileImports(path=p, imports=imps))
    return out


def rel(path: Path) -> str:
    try:
        return str(path.relative_to(repo_root()))
    except ValueError:
        return str(path)


@pytest.fixture
def archlib() -> SimpleNamespace:
    return SimpleNamespace(
        repo_root=repo_root,
        python_files=python_files,
        rel=rel,
        FileImports=FileImports,
    )
