---
name: ac-to-spec
description: Convert acceptance criteria, Jira tickets, Gherkin scenarios, PRDs, or any product requirements into a filled-in spec under specs/ using this repo's templates. Use when translating PM artifacts or informal asks into an add, change, migrate, or remove spec ready for implementation.
---

# AC to Spec

Translate product requirements — in any format — into a filled-in spec from
`.agents/templates/`, saved under `specs/` and grilled clean before handoff.

**Parameters**: spec kind (optional — inferred from content if omitted)

**Usage examples**:
```text
/ac-to-spec @ticket.md                    # infer kind from content
/ac-to-spec add @prd-excerpt.md           # explicit: new capability
/ac-to-spec change @gherkin.feature       # explicit: change spec
/ac-to-spec migrate                       # paste inline, explicit kind
/ac-to-spec remove @slack-thread.txt      # explicit: removal spec
```

## Spec Kinds

| Kind | Template | When to use |
|------|----------|-------------|
| `add` | `.agents/templates/add.spec.md` | Building something that doesn't exist yet |
| `change` | `.agents/templates/change.spec.md` | Modifying, enhancing, or fixing existing behavior |
| `migrate` | `.agents/templates/migrate.spec.md` | Swapping providers/libraries, restructuring, moving state |
| `remove` | `.agents/templates/remove.spec.md` | Deprecating, killing, or cleaning up existing code |

Scaffold with `make spec name=<slug> kind=<add|remove|change|migrate>` (run from a port
directory, e.g. `go/`), or write `specs/<YYYYMMDD>-<slug>.md` directly matching the
template. `specs/` is **gitignored dev memory** — never reference a spec from code,
standards, or the okf bundle; only other specs may reference specs.

## Reference knowledge

Ground technical sections in the bundle — cite concepts, never copy their content in:

- `okf/orientation/what-this-system-is.md` — what the system does; event flow context
- `okf/standards/architecture-design.md` — the authoritative design; the ingest→root→workflow path
- `okf/standards/webhooks.md` — webhook routes and check names (external contracts)
- `okf/orientation/suspend-resume-design.md` — durable park/resume design
- `okf/standards/testing.md` — coverage floor, no LLM-output assertions
- `okf/standards/documentation.md` — which okf concepts/diagrams must move with a change
- `okf/modules/agents/*.md` — per-agent concepts (fixflow, reviewer, summary, ...)

## Input Formats

Accepts any of these — no preprocessing required: **Gherkin** (`Given/When/Then`),
**Jira ticket** (title, description, AC checklist), **PRD prose**, **user stories**,
**bullet lists** from Slack/email/notes, **free text**. Extract structure from whatever
is provided.

## Subagent dispatch (multi-port / multi-agent grilling)

The grill step (step 7) runs inline. For specs spanning **more than one workflow agent
or both modern ports' internals**, dispatch one `Explore` agent IN PARALLEL with the
grill so the dialog isn't blocked on serial codebase reads:

```text
For a spec touching {agents/packages}, report per unit:
- Current shape from its okf/modules/ concept and the code itself.
- Recent changes in the spec's domain (last 5 commits to that directory).
- Env vars, webhook routes, or check names it owns.
Return findings only — do NOT modify the spec.
```

For single-agent or single-package specs, direct reads are faster than briefing a subagent.

## Steps

### 1. Accept Input
The user provides requirements via `@file` or inline paste. Read the full content.

### 2. Determine Spec Kind
If the user specified a kind, use it. Otherwise infer from content signals:

| Signal | Inferred kind |
|--------|---------------|
| "build", "create", "add new", "introduce", named capability with no existing code | `add` |
| "change", "update", "enhance", "fix", "modify", references existing behavior | `change` |
| "migrate", "swap", "upgrade", "replace X with Y", "restructure", "move state" | `migrate` |
| "remove", "deprecate", "kill", "sunset", "clean up", "delete" | `remove` |

If ambiguous, ask rather than guess — frame the question with what you detected
(e.g. "this adds a tool AND changes dispatch — one `change` spec or split?").

### 3. Read the Target Template
Read `.agents/templates/<kind>.spec.md` for the exact section structure. Keep the
header shape: `# <KIND>: <title>` and `> Kind: <kind> · Status: draft · Date: <date>`.

### 4. Read Existing Code (for `change`, `migrate`, `remove`)
Ground the spec in current state, **Go first** (the reference port):

- `change` — read the affected package/agent to describe Context accurately
- `migrate` — read the current shape to fill Context and Target state honestly
- `remove` — enumerate callers, config, and external state for Impact analysis

For `add`, check whether something similar already exists — if so, confirm with the
user whether this is really `add` or should be `change`.

### 5. Extract and Map Requirements
Map extracted information onto the template sections:

- **What/Why** → Context, Motivation
- **Behaviors** — observable outcomes → Design / Proposed change; Scope (add)
- **Before→after** — explicit deltas in signatures/config/prompts/workflow steps (change)
- **Data/state** — entities, stores, carried-over state → Design; Data / state (migrate)
- **Conditions** — edge cases, error handling → Design / Compatibility
- **Constraints** — perf, cost, out-of-scope → Scope / Compatibility / Rollback
- **Verification** — testable outcomes → Test plan (≥80% coverage, no LLM-output assertions)

Gherkin: `Given` → Context/preconditions; `When` → the triggering event or workflow
step; `Then` → Test plan assertions and Design outcomes. Jira: title → H1;
description → Context/Motivation; AC checklist → preserved verbatim plus mapped;
linked issues → Compatibility / Impact analysis.

### 6. Fill the Template
- **Fill confidently** where the input clearly grounds a section
- **Mark gaps for the grill** — tag with `<!-- TODO: [what's missing] -->`; these are
  temporary working state and MUST be resolved in step 7
- **Don't leave silent gaps** — zero-signal sections get
  `<!-- TODO: no signal in AC — [what the user must decide] -->`, never skipped
- **Don't invent** — never fabricate env var names, routes, check names, or package
  names not grounded in input or codebase; tag and grill instead
- **Preserve out-of-scope and constraints** verbatim into Scope / Compatibility
- **Preserve AC language** in Context/Motivation; translate to repo vocabulary
  (ingest, root dispatcher, workflow agents, park/resume) in Design sections
- **Leave the Checklist unchecked** — the spec describes work to be done

### 7. Grill Until Clean
Follow the grill-me skill on the working spec: scan for `<!-- TODO -->` markers, raw
template placeholder prose, `<YYYY-MM-DD>`-style header stubs, and empty plan steps;
explore the codebase before asking; one question at a time with a recommended answer
and tradeoff; write each resolution back immediately; append a `## Decisions` log.

Always walk the house decision branches, even if unmarked:
1. **External contracts** — any ingest Kind, webhook route, or check-name change is
   observed by GitHub and by every target repo's CI (`okf/standards/webhooks.md`)
2. **Env/config** — name, default, validation; `internal/config` is the only env reader
3. **Durable park/resume** — does the change interact with suspended workflows?
4. **okf bundle** — which concepts/diagrams move with this change
   (`okf/standards/documentation.md`)

**No `<!-- TODO -->` markers may survive.** Explicit deferrals go under `### Deferred`
in the Decisions log with a reason.

### 8. Append Original Requirements
At the bottom of the spec:

```markdown
---

## Original Requirements

> **Source**: [format detected]
> **Converted on**: {date}
> **Spec kind**: {kind} (inferred / explicit)

<details>
<summary>Original acceptance criteria (click to expand)</summary>

{verbatim original input, unmodified}

</details>
```

### 9. Present the Result
Save as `specs/<YYYYMMDD>-<slug>.md` and tell the user: which kind was used (and why,
if inferred); how many decisions were grilled and how many deferred (call deferred ones
out); and that the spec is implementation-ready — including which ports it covers.

## Key Rules

- **The spec is the artifact, not the AC** — usable without the original input
- **Specs are disposable dev memory** — gitignored; durable knowledge lands in okf or
  a standard after the change ships, never by referencing the spec
- **Grill until clean — never punt TODOs forward**
- **Stay grounded** — every filled section traces to input, codebase, or a grill answer
- **Use repo vocabulary** — ingest, root dispatcher, workflow agents, park/resume,
  fixflow — so the spec reads natively
- **Respect the template** — no extra sections, no skipped sections

## No Verification Needed
Read-only skill — produces a document, not code changes.
