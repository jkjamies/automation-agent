"""Import-boundary conformance tests.

Enforced rules:
  * deterministic tooling must never import agent packages;
  * provider SDKs (LiteLlm / Gemini / genai) live only in ``agent/setup``;
  * nothing imports the ``cmd`` entrypoint packages.
"""

from __future__ import annotations

import re

_TOOLING = ("auth", "githubapi", "gitrepo", "webhook", "notify", "tasks", "obs")
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


def test_only_config_reads_otel_env(archlib) -> None:
    """Only ``config`` may read the ``OTEL_*`` environment. Tracing config flows through the
    typed Config like every other setting; obs and the rest of the package take it as a
    struct, never ``os.environ.get("OTEL_...")``. A stray read elsewhere would fork
    configuration away from the single source of truth (and out of the masked-secret repr).
    Enforced by source scan: the literal ``"OTEL_`` outside ``config`` flags a direct env
    reference. (The scan covers ``automation_agent/``; the ``cmd/`` entrypoints likewise take
    the exporter from loaded config, not the environment.)"""
    base = archlib.repo_root() / "automation_agent"
    config_dir = base / "config"
    errors = []
    for fi in archlib.python_files(base):
        if fi.path.is_relative_to(config_dir):
            continue  # config owns the OTEL_* env vars
        text = fi.path.read_text()
        # Match either quote style — Python string literals use both, so a single-quoted
        # 'OTEL_...' env read must be caught as readily as a double-quoted one.
        if '"OTEL_' in text or "'OTEL_" in text:
            errors.append(
                f"{archlib.rel(fi.path)} references an OTEL_ env var literal — "
                "only config may read OTEL_*"
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
