# Python port status

Tracks each Go reference package against its Python port. Keep this honest — it is how
cross-language drift stays visible (see `../.agents/standards/language-parity.md`).

Legend: ✅ done · 🚧 in progress · ⬜ pending

| Go package | Python module | Status | Notes |
|---|---|---|---|
| `internal/config` | `automation_agent.config` | ✅ | `Config`, `load()`/`load_from()`, `validate()`, defaults — ported with tests. |
| `internal/ingest` | `automation_agent.ingest` | ✅ | `Envelope`, `Kind`, `Kind.valid()` — ported with tests. |
| `internal/notify` | `automation_agent.notify` | ✅ | `Notifier`, `Message`, Slack + Teams, `new_notifier()` — httpx; tested with respx. |
| `internal/githubapi` | `automation_agent.githubapi` | ✅ | go-github → PyGithub; pure `parse_check_run_event` + fake-`_gh` method tests. |
| `internal/gitrepo` | `automation_agent.gitrepo` | ✅ | go-git → GitPython; tested against a local seed repo. |
| `internal/scheduler` | `automation_agent.scheduler` | ✅ | robfig/cron → APScheduler (`CronTrigger.from_crontab`). |
| `internal/webhook` | `automation_agent.webhook` | ✅ | net/http → FastAPI; HMAC verify; tested with `TestClient`. |
| `internal/agent/setup` | `automation_agent.agent.setup` | ✅ | `build_llm` (LiteLlm/Gemini), prompt loader, runner, `LongRunDriver` + `Sequencer` (suspend/resume verified). |
| `internal/agent/root` | `automation_agent.agent.root` | ✅ | dispatcher by `Kind`. |
| `internal/agent/summary` | `automation_agent.agent.summary` | ✅ | parallel fetch → summarize(LLM) → notify. |
| `internal/agent/lintfixer` | `automation_agent.agent.lintfixer` | ✅ | analyze/triage; uses fixflow. |
| `internal/agent/covfixer` | `automation_agent.agent.covfixer` | ✅ | coverage analyze/triage (explore + execute); uses fixflow. |
| `internal/agent/fixflow` | `automation_agent.agent.fixflow` | ✅ | shared engine: driver, applyfix, registry, suspend/resume. |
| `cmd/agent` | `cmd/agent/main.py` | ✅ | full wiring → FastAPI (uvicorn) + APScheduler. |
| `cmd/playground` | `cmd/playground/chat` | ✅ | maps to an ADK `adk web` agent dir (`make playground`). |
| `ARCH/` | `arch` (pytest) | ✅ | `ast`-based import-boundary + AGENTS.md-presence checks; run via `make arch`. |

## Porting order (followed the Go phased roadmap)

1. ✅ Skeleton & standards — uv project, layout, `Makefile`, arch tests, per-dir `AGENTS.md`.
2. ✅ Model layer — `agent.setup` (LiteLlm for Ollama; verified suspend/resume).
3. ✅ Deterministic tooling — `githubapi`, `gitrepo`, `notify`, `webhook`, `scheduler`.
4. ✅ Root + Summary workflow.
5. ✅ fixflow + lintfixer + covfixer (suspend/resume).
6. ✅ Entrypoint wiring + parity docs.

**Testing:** pytest (asyncio auto mode); coverage via pytest-cov (`make cover`, 80% floor);
architecture via `make arch`. Never assert on LLM output content.

**Toolchain:** Python ≥3.11 (dev on 3.13), `uv`, `google-adk` (PyPI), `ruff` (lint/format),
`mypy` (type-check).

**Key library choices** (parity is functional, not library-for-library): HTTP client
**httpx**; server **FastAPI + uvicorn**; tests **respx** + FastAPI `TestClient`; cron via
**APScheduler**; git via **GitPython**; GitHub via **PyGithub**; local LLM via ADK's
built-in **`LiteLlm("ollama_chat/…")`** (adk-python has native Ollama support, so no
hand-rolled adapter is needed — unlike the Go variant).

**Intentional adaptations:** Go `(T, error)` → return value + raise; goroutines/`context`
→ `asyncio`; registry timeout `time.AfterFunc` → `loop.call_later`; tool errors self-wrap
as `{"error": …}` (Python ADK propagates tool exceptions); `embed.FS` →
`importlib.resources`. The package is installed (not run via `PYTHONPATH`) because a
top-level `cmd/` package would shadow the stdlib `cmd` module; entrypoints run by path.
