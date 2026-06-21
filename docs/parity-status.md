# Cross-port parity status — known drift

> Status: **Living record.** Last updated: 2026-06-21.

`automation-agent` is maintained as parallel ports of one design (Go · Kotlin · Python ·
TypeScript). The contract is [`.agents/standards/language-parity.md`](../.agents/standards/language-parity.md):
**Go is the source of truth**, and a behavior change should land in Go first, then
propagate to every existing port in the same logical change.

This file is the central, language-neutral record of where the ports currently **diverge
in functionality** — i.e. parity debt that still needs to be reconciled. Keep it current:
when a port gains a fix the others lack, add a row; when all ports match again, remove it.

## Open parity debt

### Fixes applied to Python first, still missing in Go and Kotlin

A 2026-06-21 review pass validated a set of findings and fixed them in the **Python** port
(commit `ffb4e67`). Because they landed Python-first rather than Go-first, Go and Kotlin
still carry the original behavior and must be brought into line (ideally re-based onto a
Go change so Go remains the reference).

| ID | Fix | Python location | Go | Kotlin |
|---|---|---|---|---|
| C2 | Notify reviewers on terminal apply failures instead of failing silently | `agent/fixflow/driver.py` (`Driver._fail`) | ❌ needs fix | ❌ needs fix |
| B2 | Return HTTP 413 on oversize webhook bodies instead of truncating (truncation fails HMAC / can feed malformed JSON downstream) | `webhook/server.py` | ❌ needs fix | ❌ needs fix (worse — no body cap at all) |
| B5 | Pin cron schedules to UTC (scheduler + trigger) | `scheduler/scheduler.py` | ❌ needs fix | ❌ needs fix |
| B9 | Log skipped/unreadable files in lint/coverage/fixflow analyze | `agent/{lintfixer,covfixer,fixflow}/analyze.py` | ❌ needs fix | ❌ needs fix |
| S3 | De-duplicate `safe_name` into a single shared helper | `agent/setup/names.py` | ❌ needs fix | ❌ needs fix |
| S6 | Cache triage on the run and reuse it across retries (avoid re-paying an identical LLM call) | `agent/fixflow/{driver,engine}.py` (`RunParams.work`) | ❌ needs fix | ❌ needs fix |
| C5 (part) | Track webhook dispatch tasks (asyncio GC hazard) and drain them on shutdown | `cmd/agent/main.py` | n/a (lang-specific) — Go still lacks bounded/drained dispatch | n/a — Kotlin still lacks drained dispatch |

Language-specific best-practice fixes in the same commit have no direct Go/Kotlin analogue
but cover the same intent (validate-early / no silent invariants): `PY4` (assert → explicit
`ValueError`), `PY7` (validate `PORT` in `Config.validate()`).

### Still open in all ports (architectural / design — do Go-first)

These were reviewed but deliberately **not** patched Python-only, because they are
architectural or security decisions that belong in the Go reference first:

- **C1** — weekly digest is identical to the daily digest (the summary graph is built once
  with a fixed 24h window + "Daily" title; needs per-fire window/title threading).
- **C3** — a run can be parked *after* its PR already exists (TOCTOU; small window).
  Closing it well wants a durable registry or a re-query of the check on resume/timeout.
- **C5 (remaining)** — full graceful shutdown: server lifecycle, a *bounded* dispatcher,
  and draining cron-dispatch coroutines.
- **PY2 / kickoff auth** — `/webhooks/lint` and `/webhooks/coverage` are unauthenticated
  and the target repo is caller-controlled; needs an auth scheme and/or a repo allowlist,
  designed once and mirrored to every port.

## Notes

- The full review with per-finding detail lives in `specs/code-review-2026-06-21.md`
  (kept local / gitignored).
- Python's `PORTING.md` was removed when the Python docs were decoupled from the Go-parity
  framing; this root-level file is the replacement record for parity debt.
