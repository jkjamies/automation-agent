"""Text-recovery helpers for parsing model output.

Pull a JSON array/object out of model output that may add prose or code fences,
and strip surrounding markdown fences from generated code.
"""

from __future__ import annotations

import json


def extract_json_array(s: str) -> str:
    """Return the first complete JSON array in model output (which may add prose or code
    fences), scanning from the first ``[`` and decoding a single value — so trailing prose
    or a stray bracket can't corrupt the span. "" if none parses."""
    return _first_json_value(s, "[")


def extract_json_object(s: str) -> str:
    """Return the first complete JSON object in model output. "" if none parses."""
    return _first_json_value(s, "{")


def _first_json_value(s: str, opener: str) -> str:
    decoder = json.JSONDecoder()
    start = s.find(opener)
    while start >= 0:
        try:
            _, end = decoder.raw_decode(s, start)
            return s[start:end]
        except json.JSONDecodeError:
            start = s.find(opener, start + 1)
    return ""


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
