"""Path-safe checkout file access.

:func:`safe_join` REJECTS (not clamps) absolute paths and any path escaping the
checkout root via ``..``. Both reads and writes route through it, so LLM-controlled
paths cannot touch host files.
"""

from __future__ import annotations

import os

__all__ = ["read_file", "safe_join"]


def safe_join(root: str, rel: str) -> str:
    """Resolve a repo-relative path against ``root``, raising on absolute paths or
    paths that escape the root via ``..``."""
    if os.path.isabs(rel):
        raise ValueError(f"absolute path {rel!r} not allowed")
    root = os.path.normpath(os.path.abspath(root))
    full = os.path.normpath(os.path.join(root, rel))
    if full != root and not full.startswith(root + os.sep):
        raise ValueError(f"path {rel!r} escapes the repo")
    # Symlink containment: a symlinked directory inside the (attacker-influenced) checkout
    # could redirect an otherwise-in-bounds path outside the root, so re-check the real,
    # symlink-resolved location. realpath resolves the existing prefix and appends any
    # not-yet-created tail (e.g. a new test file) literally; both sides are resolved so a
    # symlinked temp root (/var -> /private/var on macOS) doesn't cause a false reject.
    root_real = os.path.realpath(root)
    full_real = os.path.realpath(full)
    if full_real != root_real and not full_real.startswith(root_real + os.sep):
        raise ValueError(f"path {rel!r} escapes the repo via a symlink")
    return full


def read_file(root: str, rel: str) -> str:
    """Read a repo-relative file from the checkout (path-safe)."""
    full = safe_join(root, rel)
    with open(full, encoding="utf-8") as f:
        return f.read()
