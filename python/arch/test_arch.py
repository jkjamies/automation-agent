"""Import-boundary conformance tests.

Enforced rules:
  * deterministic tooling must never import agent packages;
  * provider SDKs (LiteLlm / Gemini / genai) live only in ``agent/setup``;
  * nothing imports the ``cmd`` entrypoint packages.
"""

from __future__ import annotations

import re

_TOOLING = ("auth", "githubapi", "gitrepo", "webhook", "notify")
_AGENT_PREFIX = "automation_agent.agent"
_PROVIDER_PAT = re.compile(
    r"(litellm|lite_llm|adk\.models\.gemini|google\.adk\.models\.Gemini|google\.genai)"
)


def _under(path, base, *pkgs: str) -> bool:
    for pkg in pkgs:
        target = base / pkg
        if str(path).startswith(str(target)):
            return True
    return False


def test_tooling_does_not_import_agents(archlib) -> None:
    base = archlib.repo_root() / "automation_agent"
    errors = []
    for fi in archlib.python_files(base):
        if not _under(fi.path.parent, base, *_TOOLING):
            continue
        for imp in fi.imports:
            if imp.startswith(_AGENT_PREFIX):
                errors.append(
                    f"{archlib.rel(fi.path)} imports agent package {imp} — "
                    "tooling must not depend on agents"
                )
    assert not errors, "\n".join(errors)


def test_provider_sdks_only_in_setup(archlib) -> None:
    setup_dir = archlib.repo_root() / "automation_agent" / "agent" / "setup"
    errors = []
    for fi in archlib.python_files(archlib.repo_root() / "automation_agent"):
        if str(fi.path).startswith(str(setup_dir)):
            continue
        for imp in fi.imports:
            if _PROVIDER_PAT.search(imp):
                errors.append(
                    f"{archlib.rel(fi.path)} imports provider SDK {imp} outside agent/setup"
                )
    assert not errors, "\n".join(errors)


def test_nothing_imports_cmd(archlib) -> None:
    errors = []
    for fi in archlib.python_files(archlib.repo_root()):
        if archlib.rel(fi.path).startswith("cmd/"):
            continue
        for imp in fi.imports:
            if imp == "cmd" or imp.startswith("cmd."):
                errors.append(f"{archlib.rel(fi.path)} imports cmd package {imp}")
    assert not errors, "\n".join(errors)
