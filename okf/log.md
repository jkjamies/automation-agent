# Bundle Update Log

## 2026-07-04
* **Update**: Owning concepts gained *Why* sections capturing the rationale behind the
  load-bearing choices — the parity pairs, the execution transport, the durable session
  store, the model provider, and GitHub App auth — so reasoning survives the disposable
  specs where it was argued.
* **Creation**: Added [OKF format](/standards/okf-format.md) — the bundle's own format
  contract (frontmatter schema, type taxonomy, timestamp semantics, link convention,
  the conformance floor).
* **Creation**: Added [Security model](/standards/security.md) — the service's security
  controls in one registry view.
* **Update**: The [glossary](/orientation/glossary.md) gained the bundle's own
  vocabulary (bundle, concept, conformance tests).
* **Creation**: Added [Agent skills](/tooling/skills.md) — the repo gained a skills
  system (`.agents/skills/`) encoding the spec → grill → implement → verify → knowledge
  workflow; skills cite this bundle one-way, and the conformance tests validate every
  citation.
* **Initialization**: Established the bundle as the canonical knowledge layer for
  `automation-agent` — orientation, standards, modules (agents / platform / ports), and
  tooling — replacing the per-directory `AGENTS.md` tree and `.agents/standards/` as the
  home of system knowledge. The repo-root `AGENTS.md` remains as the discovery pointer
  and guardrail sheet.
