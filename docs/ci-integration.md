# CI Integration — sending lint problems to the agent

This guide shows how a CI workflow on **any** tech stack calls the automation-agent's
lint-fixer. The agent is format-agnostic: your CI sends whatever its linter emits,
and an LLM triage step normalizes it (see `docs/architecture.md` §8). You only need
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
    CI->>Agent: POST /webhooks/lint (202)
    Agent->>Agent: triage (LLM) + analyze per file
    Agent->>GH: open PR on automation-agent/lint-fix + label
    GH->>Verify: pull_request [labeled, synchronize]
    Verify->>GH: check_run completed (success/failure + findings)
    GH->>Agent: POST /webhooks/github (check_run, HMAC)
    Agent->>Agent: success -> notify; failure & attempts<3 -> retry; >=3 -> needs review
```

---

## Examples (GitHub Actions)

Each example assumes two secrets/vars: `AGENT_URL` (the agent base URL) and an
optional shared secret if you front the webhook with auth. Set `MAX_LINT_ERRORS`
as desired (default 5).

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
workflow to each target repo that runs your lint as a dedicated, label-triggered
check and writes the findings into the check output (which the agent re-triages from
on a retry):

```yaml
name: agent-lint-verify
on:
  pull_request:
    types: [labeled, synchronize]   # labeled = first run; synchronize = each retry push
jobs:
  agent-lint-verify:
    if: contains(github.event.pull_request.labels.*.name, 'automation-agent')
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

- **success** → posts a success summary to Slack/Teams,
- **failure & attempts < `MAX_ITERATIONS` (3)** → re-analyzes with the check output as
  feedback and pushes onto the same branch,
- **failure & attempts ≥ 3** → posts "needs human review" + the PR link.

Configure the webhook on each repo (Settings → Webhooks): payload URL
`https://<agent-host>/webhooks/github`, content type `application/json`, secret =
`GITHUB_WEBHOOK_SECRET`, events = **Check runs**.
