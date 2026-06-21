"""Read-only repository tools for a tool-using agent.

Port of ``fixflow/tools.go``: :func:`repo_tools` returns ``read_file`` and ``list_dir``
rooted at the checkout, so an agent can examine the real repository — its standards
docs, existing tests, and layout — and ground decisions in what the repo actually does.
Both tools are path-safe via :func:`_safe_join`.
"""

from __future__ import annotations

import os

from google.adk.tools import BaseTool, FunctionTool

from automation_agent.agent.fixflow.files import _safe_join, read_file


def list_dir_entries(root: str, rel: str) -> list[str]:
    """List a checkout directory (path-safe), suffixing subdirectories with ``/`` and
    hiding the ``.git`` directory. Entries are sorted."""
    full = _safe_join(root, rel)
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
        return {"content": read_file(root, path)}

    def list_dir_tool(path: str) -> dict:
        """List the files and subdirectories of a repository directory by its
        repo-relative path. Use "." for the repository root."""
        return {"entries": list_dir_entries(root, path)}

    read_file_tool.__name__ = "read_file"
    list_dir_tool.__name__ = "list_dir"
    return [FunctionTool(read_file_tool), FunctionTool(list_dir_tool)]
