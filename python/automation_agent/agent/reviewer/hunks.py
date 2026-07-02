"""Which head-side lines of a patch GitHub accepts an inline comment on, used to route a finding
to an inline comment vs. the summary's out-of-diff section."""

from __future__ import annotations

from automation_agent.githubapi import PRFile


def commentable_lines(patch: str) -> set[int]:
    """Return the new-side (head) line numbers in a unified-diff patch that GitHub will accept a
    RIGHT-side inline comment on: added ('+') and context (' ') lines. Removed ('-') lines have
    no head-side line and are skipped. A malformed or empty patch yields an empty set, so a
    finding on it is treated as out-of-diff rather than posted at a wrong line."""
    out: set[int] = set()
    new_line = 0
    in_hunk = False
    for line in patch.split("\n"):
        if line.startswith("@@"):
            new_line, in_hunk = _parse_hunk_new_start(line)
            continue
        if not in_hunk:
            continue
        if line.startswith("+"):
            out.add(new_line)
            new_line += 1
        elif line.startswith("-"):
            pass  # removed line: advances the old side only, no head-side line
        elif line.startswith(" "):
            out.add(new_line)
            new_line += 1
        elif line.startswith("\\"):
            pass  # "\ No newline at end of file": metadata, not a line
        else:
            # a blank or unexpected line ends this hunk's body
            in_hunk = False
    return out


def _parse_hunk_new_start(header: str) -> tuple[int, bool]:
    """Parse the new-file starting line from a hunk header "@@ -a,b +c,d @@", returning
    ``(c, True)``. A header it cannot parse yields ``(0, False)`` so the body until the next
    header is skipped rather than mis-numbered."""
    plus = header.find("+")
    if plus < 0:
        return 0, False
    rest = header[plus + 1 :]
    end = _index_any(rest, " ,")
    if end >= 0:
        rest = rest[:end]
    try:
        n = int(rest)
    except ValueError:
        return 0, False
    if n <= 0:
        return 0, False
    return n, True


def _index_any(s: str, chars: str) -> int:
    """Return the index of the first character in ``s`` that is in ``chars``, or -1."""
    for i, ch in enumerate(s):
        if ch in chars:
            return i
    return -1


class DiffIndex:
    """Maps each changed file to the head-side lines an inline comment can target."""

    def __init__(self, files: list[PRFile]) -> None:
        """Build the in-diff line index for a set of changed files."""
        self._idx: dict[str, set[int]] = {f.path: commentable_lines(f.patch) for f in files}

    def in_diff(self, file: str, line: int) -> bool:
        """Report whether file:line falls on a commentable head-side line of the diff."""
        lines = self._idx.get(file)
        return lines is not None and line in lines
