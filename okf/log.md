# Bundle Update Log

## 2026-07-04
* **Creation**: Added the [decisions](/decisions/index.md) layer — six records capturing
  why the load-bearing choices were made (parity pairs, Cloud Tasks transport, Firestore
  sessions, Gemini-on-Vertex, GitHub App auth & native triggers, the bundle itself), so
  rationale survives the disposable specs where it was argued.
* **Creation**: Added [OKF format](/standards/okf-format.md) — the bundle's own format
  contract (frontmatter schema, type taxonomy including `Decision`, timestamp semantics,
  link convention, the conformance floor).
* **Creation**: Added [Security model](/standards/security.md) — the service's security
  controls in one registry view.
* **Update**: The [glossary](/orientation/glossary.md) gained the bundle's own
  vocabulary (bundle, concept, decision record, conformance tests).
* **Creation**: Added [Agent skills](/tooling/skills.md) — the repo gained a skills
  system (`.agents/skills/`) encoding the spec → grill → implement → verify → knowledge
  workflow; skills cite this bundle one-way, and the conformance tests validate every
  citation.
* **Initialization**: Established the bundle as the canonical knowledge layer for
  `automation-agent` — orientation, standards, modules (agents / platform / ports), and
  tooling — replacing the per-directory `AGENTS.md` tree and `.agents/standards/` as the
  home of system knowledge. The repo-root `AGENTS.md` remains as the discovery pointer
  and guardrail sheet.
