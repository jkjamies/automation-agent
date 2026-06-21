"""Text-recovery helpers for parsing model output.

Pull a JSON array/object out of model output that may add prose or code fences,
and strip surrounding markdown fences from generated code.
"""

from __future__ import annotations


def extract_json_array(s: str) -> str:
    """Return the substring from the first ``[`` to the last ``]``, so a JSON array
    can be recovered from model output that adds prose or code fences. "" if none."""
    i = s.find("[")
    j = s.rfind("]")
    if i < 0 or j < 0 or j < i:
        return ""
    return s[i : j + 1]


def extract_json_object(s: str) -> str:
    """Return the substring from the first ``{`` to the last ``}``. "" if none."""
    i = s.find("{")
    j = s.rfind("}")
    if i < 0 or j < 0 or j < i:
        return ""
    return s[i : j + 1]


def strip_fences(out: str) -> str:
    """Remove surrounding markdown code fences a model may add and normalize a
    trailing newline."""
    s = out.strip()
    if s.startswith("```"):
        nl = s.find("\n")
        if nl >= 0:
            s = s[nl + 1 :]
        j = s.rfind("```")
        if j >= 0:
            s = s[:j]
    return s.rstrip("\n") + "\n"
