---
name: grill-me
description: Interview the user one question at a time to resolve every unfilled marker in a spec file (`<!-- TODO -->`, raw template placeholder prose, `<YYYY-MM-DD>`-style header stubs, empty plan steps, unconfirmed reverse-spec presumptions). Use on hand-authored specs, stale specs, or when the user says "grill me" or "stress-test this plan". The conversion skills (/ac-to-spec, /reverse-spec) embed this logic inline, so freshly-converted specs don't need a separate pass. Recommends an answer for every question, explores the codebase and okf bundle instead of asking when it can, and writes resolutions back with a Decisions log.
---

# Grill Me

Stress-test a spec by walking every unresolved decision branch with the user, one
question at a time, until the spec is implementation-ready.

The spec is the input AND the output. This skill mutates the spec file in place.

**Parameters**: `@spec-file.md` (required)

**Usage examples**:
```text
/grill-me @specs/20260704-reviewer-batching.md
/grill-me @specs/20260628-cloud-tasks-transport.md   # any kind (add/change/migrate/remove)
```

## When to Use

| Trigger | Why |
|---------|-----|
| Hand-authored spec from a template | Catch unfilled placeholders before implementation |
| Stale spec — previously grilled, but the world changed | Re-resolve only the now-stale sections |
| User says "grill me" / "stress-test this plan" / "poke holes" | Explicit invocation |
| Consumer flow detected unresolved markers pre-implementation | Safety net |

**Note**: `/ac-to-spec` and `/reverse-spec` embed this same grill inline — running
`/grill-me` on a freshly converted spec is a no-op.

## When NOT to Use

- Spec is fully filled with no markers — nothing to grill
- User wants exploratory discussion, not decision-resolution — use chat
- Code is already written — use `/reverse-spec` to derive the spec from the code

## Reference knowledge

Explore these before asking — cite the concept, never inline its content:

- `okf/standards/language-parity.md` — two-pair contract; Go-first workflow rule
- `okf/standards/architecture-design.md` — the language-neutral design and boundaries
- `okf/standards/webhooks.md` — webhook routes + check names (all-four-port contracts)
- `okf/orientation/suspend-resume-design.md` — durable park/resume mechanics
- `okf/standards/testing.md` — coverage floor, no LLM-output assertions
- `okf/standards/documentation.md` — which okf concepts/diagrams move with a change
- `okf/modules/agents/*.md`, `okf/modules/platform/`, `okf/modules/ports/` — unit concepts

## Steps

### 1. Read the Spec
Read the file passed via `@`. If none is passed, ask which spec — do not invent one.

Detect the kind from the H1 (`# ADD: ...`, `# CHANGE: ...`, `# MIGRATE: ...`,
`# REMOVE: ...`) and read the matching `.agents/templates/<kind>.spec.md` so you know
which sections are required and what the placeholder prose looks like.

### 2. Scan for Unresolved Markers
Walk the spec top-to-bottom and collect every unresolved item:

| Marker | Meaning | Example |
|--------|---------|---------|
| `<!-- TODO: ... -->` | A conversion skill flagged a gap | `<!-- TODO: no signal in AC -->` |
| Placeholder prose verbatim from the template | Section never filled | "What exists today, and why this addition is needed." |
| `<...>` stubs in the H1 or header line | Identity never set | `# ADD: <feature name>`, `Date: <YYYY-MM-DD>` |
| Empty numbered steps in Migration plan / Removal plan | Plan not written | `1.` with no body |
| Empty bullets in Scope / Impact analysis | List started, never filled | `- In scope:` with nothing after |
| `presumably` / `appears to` / `unclear why` | Reverse-spec inference, unconfirmed | see /reverse-spec |

Build an ordered list. Order matters — see step 3. (Unchecked `- [ ]` Checklist items
are NOT markers — the checklist describes work to be done.)

### 3. Order the Branches
Resolve dependencies before dependents:

1. **Identity** — H1 title, kind, date (everything hangs off these)
2. **Context & Motivation** — drives all technical decisions
3. **Scope** — in/out of scope (add); Target state (migrate); Impact analysis (remove)
4. **Parity impact** — modern pair only? Go-first then Python in the same logical
   change; frozen Kotlin/TS untouched by features. If a critical fix must touch the
   frozen pair, it lands in BOTH frozen ports
5. **External contracts** — ingest Kind, webhook route, or check-name changes bind all
   four ports; anything another system observes
6. **Design / Proposed change** — packages, agents, tools, the ingest→root→workflow
   path touched, prompts
7. **Env/config** — new or changed env vars: identical names, defaults, validation
   across the pair
8. **Durable park/resume** — does the change affect suspended workflows, resume keys,
   or the CI-wait loop?
9. **Test plan** — coverage stays ≥80%; never assert on LLM output
10. **Rollout / rollback** — and per-step reversibility for migrate
11. **okf bundle updates** — which concepts/diagrams must move with the change

### 4. Explore Before Asking
For each marker, first decide: **can the codebase or bundle answer this?**

| Marker | Look here |
|--------|-----------|
| Env var name/default/validation | `.env.example`, Go config package (Go is the reference) |
| Webhook route / check name | `okf/standards/webhooks.md`, ingest package |
| Agent topology / where logic lives | `okf/standards/architecture-design.md`, `okf/modules/agents/` |
| Whether the frozen pair is affected | `okf/standards/language-parity.md` (external contracts vs features) |
| Park/resume interaction | `okf/orientation/suspend-resume-design.md`, session store code |
| Prompt content/location | `prompts/` markdown in the affected port |
| Naming/structure conventions | the sibling package in Go and its bundle concept |
| Which docs/diagrams move | `okf/standards/documentation.md` |

When grounded, present a confirmation rather than a question:

> Checked `.env.example` and the Go config package — there's no `REVIEW_BATCH_SIZE`
> yet. I recommend adding it with default `5`, validated ≥1, mirrored in Python.
> Sound good, or reuse an existing knob?

If the codebase doesn't answer it (product intent, priority, business rule), ask.

### 5. Interview — One Question at a Time
For each remaining marker:

1. **State the context** — which section, why it matters, what depends on it
2. **Recommend an answer** — grounded in conventions, siblings, the bundle
3. **Surface the tradeoff** — one sentence on what changes if they pick differently
4. **Wait** — never batch; do not move on until this branch is resolved

> **[Section] — [Field]**
> [1-sentence why this matters and what it blocks]
>
> **Recommendation**: [your answer]
> **Tradeoff**: [what changes if they pick differently]
>
> Accept, or override?

If the user says "your call", accept the recommendation and move on.

### 6. Write Back As You Go
After each resolution, immediately Edit the spec — replace the marker with the answer,
one marker per write. The spec stays in a valid intermediate state; an interrupted
grill loses nothing.

### 7. Append a Decisions Log
When all markers are resolved (or explicitly deferred), append before any existing
`## Original Requirements` section:

```markdown
---

## Decisions

> Resolved during /grill-me on {date}. Each entry: question → answer → rationale.

### [Section name]
- **[Field]** — [answer]
  - **Rationale**: [why; cite codebase/okf evidence or user input]
  - **Tradeoff accepted**: [if relevant]
```

Deferred items go in a `### Deferred` subsection with the reason and what would
unblock them — never silently left in place.

### 8. Final Pass
Re-run the step-2 scan. If undeclared markers remain, resume grilling. When clean,
summarize: decisions resolved, deferrals (and why), and the suggested next step
(implement Go first, mirror Python; note any all-four-port contract work).

## Stop Condition
All markers are either **resolved (written into the spec)** or **explicitly deferred
(logged in Decisions)**. Mechanical, not vibes-based: an empty step-2 scan
(excluding deferred entries) means done.

## Key Rules

- **One question at a time** — never batch; each answer informs the next question
- **Explore before asking** — test every question against the codebase and okf first
- **Always recommend** — the user accepts, overrides, or defers; never designs from scratch
- **Write as you go** — the spec is always the source of truth
- **The spec is the artifact** — the Decisions log lives inside it, no side transcript
- **Don't drift** — tangents become deferred notes; return to the marker list
- **Honor deferral** — don't re-grill deferred items in the same session
- **Don't re-decide** — filled text is settled; only target genuine markers
- **No invention** — never make up routes, env vars, or business rules; defer instead
- **Specs stay dev memory** — never add references to this spec into code, standards,
  or the okf bundle while grilling

## No Verification Needed
Read/write on a single document — no code changes, no verify step. The implementation
that follows runs its own gates (`make ci`, `make arch`).
