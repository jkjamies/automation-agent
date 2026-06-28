# cmd/agent

The service entrypoint. Responsibilities (built out across phases):

## Flow

```mermaid
flowchart TD
    Main["main(): logging setup -> asyncio.run(run())"] --> Env["load .env (optional, via python-dotenv)"]
    Env --> Cfg["config.load()"]
    Cfg -->|err| Fatal["raise -> exit(1)"]
    Cfg --> LLM["setup.build_llm(cfg) + build_code_llm(cfg)"]
    LLM --> GH["githubapi.Client(cfg.github_token)"]
    GH --> Notif["build_notifier(cfg) -> notify.new_notifier (or None)"]
    Notif --> SumA["build_summary_agent (None if no repos/notifier)"]
    SumA --> Eng["lintfixer.new_engine(FixDeps)<br/>covfixer.new_engine(FixDeps)<br/>engines = [lint, cov]"]
    Eng --> Disp["root.build_root_dispatcher(Deps{<br/>summary_daily,<br/>lint_kickoff=lint_engine.kickoff,<br/>coverage_kickoff=cov_engine.kickoff,<br/>ci_resume=resume to every engine})"]
    Disp -->|err| Fatal

    Disp --> Trans["build_transport(cfg, dispatcher.dispatch) -> tasks.Transport<br/>(TASKS_BACKEND: inprocess | cloudtasks)"]
    Trans --> Web["Server(ingest -> transport.enqueue,<br/>secret=github_webhook_secret,<br/>dispatch=dispatcher.dispatch, sweep=...)"]
    Web --> HTTP["FastAPI app + uvicorn (port)"]

    HTTP --> Listen["await server.serve() (uvicorn)"]
    Listen --> Block["block until interrupted"]
    Block --> Shutdown["transport.close() (drain / close client)<br/>park_store.close()"]
```

1. Load `config`.
2. Build the LLMs (`automation_agent/agent/setup`), tooling, and the
   root + summary agents plus the lint-fixer and coverage-fixer `fixflow` engines.
3. Build the execution transport (`build_transport` → `tasks.Transport`, selected by
   `TASKS_BACKEND`), then start the webhook HTTP server (FastAPI + uvicorn). The webhook
   `IngestFunc` **enqueues** on the transport and returns fast; `dispatch=` wires the
   dispatcher to `/internal/dispatch` (the Cloud Tasks worker that runs the workflow
   in-request). The daily digest is driven by Cloud Scheduler calling
   `POST /internal/cron/daily`; the service runs no internal timer.
4. Block until interrupted, then `transport.close()` (the in-process backend drains
   in-flight dispatches; the Cloud Tasks backend closes its client) followed by the park
   store close.

The fix loop suspends across the CI wait (ADK long-running suspend/resume). Both the ADK
session and the parked run are persisted through `SESSION_BACKEND` (`memory` | `sqlite` |
`firestore`) via `setup.ParkStore`, so a durable backend resumes in-flight runs after a
restart (the default `memory` backend stays ephemeral). Each wait is freed by a per-run
`CI_TIMEOUT` timer and the durable `/internal/sweep` catch-all (driven by Cloud Scheduler).

Keep this module thin — it is composition only. Anything testable belongs in
`automation_agent/`.
