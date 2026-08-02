# Bundle Update Log

## 2026-08-01
* **Update**: The bundle targets Open Knowledge Format v0.2, declared as
  `okf_version: "0.2"` in the bundle-root `index.md`. Every concept's `timestamp` became
  the v0.2 `generated: { by, at }` family — same instant, now carrying the actor that
  produced it — and gained a lifecycle `status`. The v0.2 families the bundle does not
  populate (`sources`, `verified`, `stale_after`, `Attested Computation`) are recorded in
  [OKF format](/standards/okf-format.md) with the reason each is omitted.
* **Update**: [OKF format](/standards/okf-format.md) now opens with the version the
  bundle targets and frames the rest as this bundle's house profile of that spec — the
  frontmatter schema, the actor convention for `generated.by`, the lifecycle set, the
  `log.md` entry prefixes, and the widened conformance floor.

## 2026-07-29
* **Rewrite**: [Go style](/standards/go-style.md) now adopts
  [Google's Go Style Guide](https://google.github.io/styleguide/go/) as the baseline and
  records where each layer of it is enforced — linter, architecture test, or review — rather
  than restating it. What the guide's own documents rule on became links; three deltas
  survive (visibility, dependency admission, configuration), each grounded in the guide
  section it instantiates.
* **Update**: [Architecture rules](/standards/architecture.md) gained the visibility
  conformance rule the `ARCH/` suite now enforces: an exported identifier must appear in its
  package's exported signature surface or be referred to by another package.
* **Update**: Owning concepts gained *Why* sections capturing the rationale behind the
  load-bearing choices — the execution transport, the durable session store, the model
  provider, and GitHub App auth — so reasoning survives the disposable specs where it was
  argued.
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
  `automation-agent` — orientation, standards, modules (agents / platform), and
  tooling — replacing the per-directory `AGENTS.md` tree and `.agents/standards/` as the
  home of system knowledge. The repo-root `AGENTS.md` remains as the discovery pointer
  and guardrail sheet.
