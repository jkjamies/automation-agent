"""Read-only repository tools for a tool-using agent.

:func:`repo_tools` returns ``read_file`` and ``list_dir``
rooted at the checkout, so an agent can examine the real repository — its standards
docs, existing tests, and layout — and ground decisions in what the repo actually does.
Both tools are path-safe via :func:`safe_join`.
"""

from __future__ import annotations

import os

from google.adk.tools import BaseTool, FunctionTool

from automation_agent.agent.fixflow.files import read_file, safe_join


def list_dir_entries(root: str, rel: str) -> list[str]:
    """List a checkout directory (path-safe), suffixing subdirectories with ``/`` and
    hiding the ``.git`` directory. Entries are sorted."""
    full = safe_join(root, rel)
    names: list[str] = []
    with os.scandir(full) as it:
        for entry in it:
            if entry.name == ".git":
                continue
            name = entry.name + "/" if entry.is_dir() else entry.name
            names.append(name)
    names.sort()
    return names


def repo_tools(root: str) -> list[BaseTool]:
    """Return read-only tools (``read_file``, ``list_dir``) rooted at the checkout."""

    def read_file_tool(path: str) -> dict:
        """Read a repository file by its repo-relative path (e.g. "src/main.go" or
        "AGENTS.md")."""
        # Self-wrap so a bad/missing path is a recoverable tool error (the model can retry),
        # not a raised exception that aborts the analyze/explore run.
        try:
            return {"content": read_file(root, path)}
        except Exception as exc:  # noqa: BLE001
            return {"error": str(exc)}

    def list_dir_tool(path: str) -> dict:
        """List the files and subdirectories of a repository directory by its
        repo-relative path. Use "." for the repository root."""
        try:
            return {"entries": list_dir_entries(root, path)}
        except Exception as exc:  # noqa: BLE001
            return {"error": str(exc)}

    read_file_tool.__name__ = "read_file"
    list_dir_tool.__name__ = "list_dir"
    return [FunctionTool(read_file_tool), FunctionTool(list_dir_tool)]
