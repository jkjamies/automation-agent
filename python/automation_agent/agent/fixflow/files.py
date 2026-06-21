"""Path-safe checkout file access.

:func:`_safe_join` REJECTS (not clamps) absolute paths and any path escaping the
checkout root via ``..``. Both reads and writes route through it, so LLM-controlled
paths cannot touch host files.
"""

from __future__ import annotations

import os


def _safe_join(root: str, rel: str) -> str:
    """Resolve a repo-relative path against ``root``, raising on absolute paths or
    paths that escape the root via ``..``."""
    if os.path.isabs(rel):
        raise ValueError(f"absolute path {rel!r} not allowed")
    full = os.path.normpath(os.path.join(root, rel))
    if full != root and not full.startswith(root + os.sep):
        raise ValueError(f"path {rel!r} escapes the repo")
    return full


def read_file(root: str, rel: str) -> str:
    """Read a repo-relative file from the checkout (path-safe)."""
    full = _safe_join(root, rel)
    with open(full, encoding="utf-8") as f:
        return f.read()
