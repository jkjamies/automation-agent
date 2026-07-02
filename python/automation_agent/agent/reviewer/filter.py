"""The exclude-glob file filter that drops generated/vendored/binary churn and totals the
filtered patch bytes.

Filtering first is the biggest cheap win: most "huge" PRs are mostly lockfile/vendor churn and
shrink to a handful of real files, so size is computed on the *filtered* set.
"""

from __future__ import annotations

import posixpath
import re
from dataclasses import dataclass

from automation_agent.githubapi import PRFile


@dataclass
class _GlobPattern:
    """One compiled exclude glob. A pattern with no '/' matches against the file's basename
    (e.g. "*.min.js", "go.sum"); a pattern with a '/' matches against the full path (e.g.
    "vendor/**"). "**" matches across path separators; "*" and "?" do not."""

    re: re.Pattern[str]
    basename: bool


class FileFilter:
    """Drops changed files that are not worth reviewing — generated code, vendored trees,
    lockfiles, minified bundles, snapshots, and binaries — before any size accounting or model
    call."""

    def __init__(self, globs: list[str]) -> None:
        """Compile the exclude globs. Blank entries (e.g. a trailing comma in the env value) are
        skipped. Every glob compiles — :func:`glob_to_regexp` escapes all regexp metacharacters
        — so this cannot fail."""
        self._patterns: list[_GlobPattern] = []
        for g in globs:
            g = g.strip()
            if g == "":
                continue
            self._patterns.append(_GlobPattern(re=glob_to_regexp(g), basename="/" not in g))

    def excluded(self, p: str) -> bool:
        """Report whether a path matches any exclude glob."""
        base = posixpath.basename(p)
        for pat in self._patterns:
            target = base if pat.basename else p
            if pat.re.match(target):
                return True
        return False

    def apply(self, files: list[PRFile]) -> tuple[list[PRFile], int]:
        """Return the kept (non-excluded) files and the total size of their patches in bytes.
        Size is computed on the filtered set so the size gate sees real review surface, not
        churn; files whose patch GitHub omitted are charged conservatively (see
        :func:`patch_bytes`) so an oversized PR cannot undercount its way past the byte cap."""
        kept: list[PRFile] = []
        diff_bytes = 0
        for fl in files:
            if self.excluded(fl.path):
                continue
            kept.append(fl)
            diff_bytes += patch_bytes(fl)
        return kept, diff_bytes


# The per-changed-line byte estimate charged when GitHub omits a file's patch for an oversized
# text diff. A unified-diff line is its content plus a one-char +/- prefix and a newline; real
# source lines average well above this, so the estimate is deliberately conservative: the size
# gate must over-, never under-, charge an omitted diff so a very large PR cannot slip the byte
# cap by changing files too big for GitHub to diff.
AVG_DIFF_LINE_BYTES = 50


def patch_bytes(fl: PRFile) -> int:
    """The diff-byte cost charged for one kept file. When GitHub returns the patch it is the
    exact byte length. When GitHub omits it for an oversized text file (empty patch but non-zero
    line counts) it is estimated from the reported additions+deletions. Binary files (no patch,
    no line counts) cost nothing."""
    if fl.patch != "":
        return len(fl.patch.encode("utf-8"))
    lines = fl.additions + fl.deletions
    if lines > 0:
        return lines * AVG_DIFF_LINE_BYTES
    return 0


_METACHARS = set(".+()|[]{}^$\\")


def glob_to_regexp(glob: str) -> re.Pattern[str]:
    """Compile a glob into an anchored regexp. "**" becomes ".*" (crosses path separators), "*"
    becomes "[^/]*" and "?" becomes "[^/]" (within one segment); every other regexp
    metacharacter is escaped so it matches literally. Because all metacharacters are either
    escaped or rewritten, the result is always a valid pattern."""
    b = ["^"]
    i = 0
    n = len(glob)
    while i < n:
        c = glob[i]
        if c == "*":
            if i + 1 < n and glob[i + 1] == "*":
                b.append(".*")
                i += 1  # consume the second '*'
            else:
                b.append("[^/]*")
        elif c == "?":
            b.append("[^/]")
        elif c in _METACHARS:
            b.append("\\")
            b.append(c)
        else:
            b.append(c)
        i += 1
    b.append("$")
    return re.compile("".join(b))
