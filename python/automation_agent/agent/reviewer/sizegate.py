"""The two-dimensional file-count / diff-byte size cap."""

from __future__ import annotations


def oversize(
    file_count: int, diff_bytes: int, max_files: int, max_diff_bytes: int
) -> tuple[str, bool]:
    """Report whether a filtered diff exceeds either configured cap. The gate is
    two-dimensional: a PR is too large if it changes more than ``max_files`` files OR its
    filtered patches exceed ``max_diff_bytes`` — review-or-deny, no degrade tier. A non-positive
    cap disables that dimension. The reason is phrased for the "too large — please split" deny
    comment. The size is taken on the *filtered* set, so excluded lockfile/vendor churn never
    trips the gate."""
    if max_files > 0 and file_count > max_files:
        return (
            f"{file_count} changed files (after excluding generated files) exceeds the "
            f"{max_files}-file review limit",
            True,
        )
    if max_diff_bytes > 0 and diff_bytes > max_diff_bytes:
        return (
            f"{diff_bytes} diff bytes (after excluding generated files) exceeds the "
            f"{max_diff_bytes}-byte review limit",
            True,
        )
    return "", False
