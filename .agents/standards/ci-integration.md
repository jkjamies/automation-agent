# CI Integration — sending lint problems to the agent

This guide shows how a CI workflow on **any** tech stack calls the automation-agent's
lint-fixer. The agent is format-agnostic: your CI sends whatever its linter emits,
and an LLM triage step normalizes it (see `.agents/standards/architecture-design.md` §8). You only need
to send a small, trusted envelope.

## The kickoff contract

`POST` to `https://<agent-host>/webhooks/lint` with this JSON:

```json
{
  "repo": "owner/name",
  "base": "main",
  "report": <whatever your linter emitted>
}
```

- `repo` / `base` are **trusted** — set by *your* CI, never inferred by the model.
  `repo` is `owner/name`; `base` is the branch to fix against (defaults to `main`).
- `report` is **arbitrary** — JSON, SARIF, or plain text. The agent's triage LLM
  reads it; you do not need to normalize it.

The endpoint returns `202 Accepted` immediately and works asynchronously (the agent
opens a PR, then waits for CI — see [the resume side](#the-resume-side-agent-lint-verify)).

## Authenticating the kickoff (required when `GITHUB_WEBHOOK_SECRET` is set)

A kickoff selects a caller-supplied `repo` and drives an LLM run that pushes to it and
opens a PR — so when the agent is configured with `GITHUB_WEBHOOK_SECRET`, the
**`/webhooks/lint` and `/webhooks/coverage` kickoffs are HMAC-authenticated with the
same secret as `/webhooks/github`**. An unsigned or wrong-signature request gets `401`.

Sign the **exact request body** with HMAC-SHA256 and send the digest in the
`X-Hub-Signature-256: sha256=<hex>` header (identical to GitHub's webhook scheme):

```bash
# body is the JSON you POST; AGENT_SECRET == the agent's GITHUB_WEBHOOK_SECRET
sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$AGENT_SECRET" | awk '{print $2}')"
curl -sf -X POST "$AGENT_URL/webhooks/lint" \
  -H 'content-type: application/json' \
  -H "X-Hub-Signature-256: $sig" \
  --data "$body"
```

If the agent runs **without** `GITHUB_WEBHOOK_SECRET` (local dev only), verification is
skipped and the header is not required — but do not run unauthenticated where the
kickoff endpoints are reachable, since anyone could drive a PR on any repo the agent's
`GITHUB_TOKEN` can reach. The per-language examples below omit the header for brevity;
add it as shown above whenever a secret is configured.

## Cap the number of problems (`MAX_LINT_ERRORS`, default 5)

**Send at most `MAX_LINT_ERRORS` problems per kickoff (default 5).** Why:

- Keeps the LLM prompt small, focused, fast, and cheap.
- The agent fixes per-file in parallel; a giant report fans out into too many edits
  and dilutes quality.
- A flood of problems usually means a systemic issue better handled by a human.

So your CI step should **slice the linter output to the first N problems** before
sending. Each example below does this with `jq`.

## The flow

```mermaid
sequenceDiagram
    participant CI as Your CI (lint job)
    participant Agent as automation-agent /webhooks/lint
    participant GH as GitHub
    participant Verify as agent-lint-verify check

    CI->>CI: run linter -> JSON
    CI->>CI: slice to MAX_LINT_ERRORS (jq)
    CI->>CI: build {repo, base, report}
    CI->>Agent: POST /webhooks/lint (HMAC if secret set, 202)
    Agent->>Agent: triage (LLM) + analyze per file
    Agent->>GH: open PR on automation-agent/lint-fix + label
    GH->>Verify: pull_request [labeled, synchronize]
    Verify->>GH: check_run completed (success/failure + findings)
    GH->>Agent: POST /webhooks/github (check_run, HMAC)
    Agent->>Agent: success -> notify; failure & attempts<3 -> retry; >=3 -> needs review
```

---

## Examples (GitHub Actions)

Each example assumes `AGENT_URL` (the agent base URL) and, when the agent has
`GITHUB_WEBHOOK_SECRET` set, `AGENT_SECRET` for signing the kickoff (see
[Authenticating the kickoff](#authenticating-the-kickoff-required-when-github_webhook_secret-is-set)).
Set `MAX_LINT_ERRORS` as desired (default 5). The examples omit the signature header
for brevity — add `-H "X-Hub-Signature-256: $sig"` as shown above when a secret is set.

### Go — `golangci-lint`

```yaml
name: lint-to-agent
on: [push]
env:
  MAX_LINT_ERRORS: 5
jobs:
  lint-and-notify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.26' }
      - name: Lint (JSON)
        id: lint
        run: golangci-lint run --out-format json > lint.json || true
      - name: Send up to N problems to the agent
        run: |
          report=$(jq -c '{issues: (.Issues // [])[:'"$MAX_LINT_ERRORS"']}' lint.json)
          [ "$(jq '.issues | length' <<<"$report")" -eq 0 ] && exit 0
          jq -nc --arg repo "$GITHUB_REPOSITORY" --arg base "${GITHUB_REF_NAME}" --argjson report "$report" \
            '{repo:$repo, base:$base, report:$report}' \
            | curl -sf -X POST "$AGENT_URL/webhooks/lint" -H 'content-type: application/json' --data @-
        env:
          AGENT_URL: ${{ vars.AGENT_URL }}
```

### Next.js — ESLint

```yaml
      - name: ESLint (JSON)
        run: npx eslint . --format json --output-file eslint.json || true
      - name: Send up to N problems to the agent
        run: |
          # ESLint groups by file; flatten, slice, regroup
          report=$(jq -c '[.[] | . as $f | $f.messages[] | {path:$f.filePath, line:.line, rule:.ruleId, message:.message}] | .[:'"$MAX_LINT_ERRORS"']' eslint.json)
          [ "$(jq 'length' <<<"$report")" -eq 0 ] && exit 0
          jq -nc --arg repo "$GITHUB_REPOSITORY" --arg base "$GITHUB_REF_NAME" --argjson report "$report" \
            '{repo:$repo, base:$base, report:$report}' \
            | curl -sf -X POST "$AGENT_URL/webhooks/lint" -H 'content-type: application/json' --data @-
        env:
          AGENT_URL: ${{ vars.AGENT_URL }}
          MAX_LINT_ERRORS: 5
```

### iOS — SwiftLint

```yaml
      - name: SwiftLint (JSON)
        run: swiftlint lint --reporter json > swiftlint.json || true
      - name: Send up to N problems to the agent
        run: |
          report=$(jq -c '[.[] | {path:.file, line:.line, rule:.rule_id, message:.reason}] | .[:'"$MAX_LINT_ERRORS"']' swiftlint.json)
          [ "$(jq 'length' <<<"$report")" -eq 0 ] && exit 0
          jq -nc --arg repo "$GITHUB_REPOSITORY" --arg base "$GITHUB_REF_NAME" --argjson report "$report" \
            '{repo:$repo, base:$base, report:$report}' \
            | curl -sf -X POST "$AGENT_URL/webhooks/lint" -H 'content-type: application/json' --data @-
        env:
          AGENT_URL: ${{ vars.AGENT_URL }}
          MAX_LINT_ERRORS: 5
```

### Android / Kotlin Multiplatform — detekt (or ktlint)

```yaml
      - name: detekt (SARIF)
        run: ./gradlew detekt || true   # writes build/reports/detekt/detekt.sarif
      - name: Send up to N problems to the agent
        run: |
          # SARIF: results -> {path, rule, message}; slice to N
          report=$(jq -c '[.runs[0].results[] | {path:.locations[0].physicalLocation.artifactLocation.uri, rule:.ruleId, message:.message.text}] | .[:'"$MAX_LINT_ERRORS"']' build/reports/detekt/detekt.sarif)
          [ "$(jq 'length' <<<"$report")" -eq 0 ] && exit 0
          jq -nc --arg repo "$GITHUB_REPOSITORY" --arg base "$GITHUB_REF_NAME" --argjson report "$report" \
            '{repo:$repo, base:$base, report:$report}' \
            | curl -sf -X POST "$AGENT_URL/webhooks/lint" -H 'content-type: application/json' --data @-
        env:
          AGENT_URL: ${{ vars.AGENT_URL }}
          MAX_LINT_ERRORS: 5
```

> The `report` payloads above differ in shape per stack — that's fine and the point:
> the triage LLM normalizes them. You can even send the linter's raw JSON unmodified
> (just sliced to N); the field names don't have to match anything.

---

## The resume side: `agent-lint-verify`

After the agent opens its PR (branch `automation-agent/lint-fix`, label
`automation-agent`), it **suspends** until a CI check reports back. Add **one**
workflow to each target repo that runs your lint as a dedicated, branch-gated
check and writes the findings into the check output (which the agent re-triages from
on a retry). Gate on the **branch**, not the label: every agent PR shares the single
`automation-agent` label, so the branch is what distinguishes lint from coverage runs.

```yaml
name: agent-lint-verify
on:
  pull_request:
    types: [labeled, synchronize]   # labeled = first run; synchronize = each retry push
jobs:
  agent-lint-verify:
    if: github.event.pull_request.head.ref == 'automation-agent/lint-fix'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Run the same lint and fail on remaining problems
        run: |
          # Re-run your linter here. Exit non-zero if the targeted problems remain.
          # Put the findings on stdout — GitHub captures them into the check output,
          # which the agent reads to drive the next attempt.
          golangci-lint run   # (or eslint / swiftlint / detekt for your stack)
```

The check's `check_run` event is delivered to `POST /webhooks/github` (verified with
`GITHUB_WEBHOOK_SECRET` via the `X-Hub-Signature-256` HMAC). The agent then:

- **success** → posts a status-aware success summary to Slack/Teams (commits + changed files,
  via `githubapi.Compare`, plus the original findings),
- **failure & attempts < `MAX_ITERATIONS` (3)** → re-analyzes with the check output as
  feedback and pushes onto the same branch,
- **failure & attempts ≥ 3** → posts "needs human review" + the PR link.

If the check **never reports** (a missed or never-arriving webhook), the run is freed after
`CI_TIMEOUT` (default 90m) — by an in-process per-run timer on a warm instance, and by the
durable `POST /internal/sweep` catch-all (Cloud Scheduler-driven) on the durable backends, so
a restart can't leave a run waiting forever. Either way the agent posts a timeout
"needs human review" + PR link. (Go reference; see
`.agents/standards/architecture-design.md` §8 and `DEPLOYMENT.md`.)

Configure the webhook on each repo (Settings → Webhooks): payload URL
`https://<agent-host>/webhooks/github`, content type `application/json`, secret =
`GITHUB_WEBHOOK_SECRET`, events = **Check runs**.

---

## Coverage-fixer (same pattern, different report)

The coverage-fixer works exactly like the lint-fixer but **generates tests** for
uncovered logic instead of rewriting source. It uses its own endpoint, branch,
label, and check:

| | Lint-fixer | Coverage-fixer |
|---|---|---|
| Kickoff endpoint | `POST /webhooks/lint` | `POST /webhooks/coverage` |
| Report | any linter output | any **coverage** report (JaCoCo XML, lcov, Cobertura, `go cover`, llvm-cov, SimpleCov…) |
| Branch / label | `automation-agent/lint-fix` / `automation-agent` | `automation-agent/test-coverage` / `automation-agent` |
| Verify check | `agent-lint-verify` | `agent-coverage-verify` |

**The coverage report does not need to be JSON.** Most coverage tools emit XML or
text — send the raw report as a JSON string in `report`; an LLM triage step reads any
format. Cap to `MAX_LINT_ERRORS`-style limits if the report is huge.

### Example — Go coverage

```yaml
      - name: Coverage report
        run: go test -coverprofile=cover.out ./... || true
      - name: Send coverage to the agent
        run: |
          # `go cover` is plain text; send it as a JSON string in `report`.
          jq -Rs --arg repo "$GITHUB_REPOSITORY" --arg base "$GITHUB_REF_NAME" \
            '{repo:$repo, base:$base, report:.}' cover.out \
            | curl -sf -X POST "$AGENT_URL/webhooks/coverage" -H 'content-type: application/json' --data @-
        env:
          AGENT_URL: ${{ vars.AGENT_URL }}
```

### Example — Java/Kotlin (JaCoCo XML)

```yaml
      - run: ./gradlew test jacocoTestReport || true   # build/reports/jacoco/test/jacocoTestReport.xml
      - name: Send coverage to the agent
        run: |
          jq -Rs --arg repo "$GITHUB_REPOSITORY" --arg base "$GITHUB_REF_NAME" \
            '{repo:$repo, base:$base, report:.}' build/reports/jacoco/test/jacocoTestReport.xml \
            | curl -sf -X POST "$AGENT_URL/webhooks/coverage" -H 'content-type: application/json' --data @-
        env:
          AGENT_URL: ${{ vars.AGENT_URL }}
```

(`jq -Rs` reads the raw file and emits it as a JSON string — works for XML, lcov, or
any text format.)

### The `agent-coverage-verify` check

Identical to `agent-lint-verify` but gated on the `automation-agent/test-coverage`
branch, and it should **run the tests and assert coverage rose** (or meets your
threshold), failing otherwise:

```yaml
name: agent-coverage-verify
on:
  pull_request:
    types: [labeled, synchronize]
jobs:
  agent-coverage-verify:
    if: github.event.pull_request.head.ref == 'automation-agent/test-coverage'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Run tests and enforce coverage
        run: |
          # Run the suite (incl. the agent's new tests) and fail if they don't pass
          # or coverage didn't improve. Put a short summary on stdout for the agent.
          go test ./...   # (or gradle/jest/swift test for your stack)
```

Test placement is **not** guessed from a hardcoded rule. The agent checks out the
repo, examines its **actual existing tests** (directory layout, naming, framework),
and an explorer step plans where each test belongs from that real evidence; executor
steps then write the tests. Tests are generated for **meaningful** uncovered logic
only, and imperfect output is caught and retried by the CI loop above.
