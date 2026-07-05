# Bundle Update Log

## 2026-07-04
* **Creation**: Added [Agent skills](/tooling/skills.md) — the repo gained a skills
  system (`.agents/skills/`) encoding the spec → grill → implement → verify → knowledge
  workflow; skills cite this bundle one-way, and the conformance tests validate every
  citation.
* **Initialization**: Established the bundle as the canonical knowledge layer for
  `automation-agent` — orientation, standards, modules (agents / platform / ports), and
  tooling — replacing the per-directory `AGENTS.md` tree and `.agents/standards/` as the
  home of system knowledge. The repo-root `AGENTS.md` remains as the discovery pointer
  and guardrail sheet.
