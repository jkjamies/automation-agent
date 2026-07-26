---
name: review-branch
description: The house pre-PR flow — port gates green, CodeRabbit CLI review on the branch diff, evidence-based triage, then present everything and WAIT for human approval. Use when a branch is feature-complete, before any commit/push/PR.
---

# Review Branch

Run the full house review flow on the current branch. The output is a reviewed, triaged diff presented to the human — **not** a commit, push, or PR.

**Parameters**: base (optional, default `main` — pass the stacked-base branch for stacked PRs)

**Usage examples**:
```text
/review-branch                 # review the branch diff against main
/review-branch feat/base-pr    # stacked branch: review against its base
```

**Gate definitions live in `okf/tooling/ci-gates.md`.** This skill is a thin driver.

## Steps

### 1. Gates green first

Run `/verify diff` (escalate to `/verify full` for cross-port changes). Do **not** start the CodeRabbit review while any gate is red — review noise on broken code wastes everyone's time.

### 2. CodeRabbit review

```bash
coderabbit review --agent --base main          # or --base <stacked-base>
```

**150-file cap**: the CLI refuses diffs larger than 150 files. When the diff is bigger, scope by directory and run once per touched top-level dir:

```bash
coderabbit review --agent --base main --dir go/
coderabbit review --agent --base main --dir okf/
```

### 3. Triage — fix or decline with evidence

For every finding, exactly one of:

- **Fix it** — the default for genuine findings. Apply the fix, then re-run the gate.
- **Decline with concrete evidence** — cite the file/line that proves the finding wrong, or the contract that makes the current shape deliberate (name it). Write the decline rationale down; it goes in the summary.

Never blind-accept a finding just to clear the list, and never decline with only "seems fine" — no evidence, no decline.

### 4. Re-verify after fixes

If step 3 changed code, re-run the gate. Optionally re-run CodeRabbit on the delta to confirm the fixes didn't introduce new findings.

### 5. Present and WAIT

Present to the human, then **stop**:

- The diff summary (`git diff --stat <base>...HEAD` + what/why in prose)
- Gate results per port (pass/fail, coverage numbers)
- Triage table: each finding → fixed (with commit-able change in the working tree) or declined (with the evidence)

**Do not commit, push, or open a PR.** No auto-commit is house convention — changes stay in the working tree until the human explicitly approves. Only proceed to commit/push/PR when told to.

## Key Rules

- **Order is fixed**: gates → review → triage → re-verify → present. No review on red gates.
- **Evidence or fix — no third option** for findings.
- **Declines must cite a file/line or a named contract**, never just "seems fine".
- **WAIT means wait** — presenting the triage is the end of this skill's authority.
