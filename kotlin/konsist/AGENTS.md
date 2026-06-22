# konsist (Kotlin arch module)

Architecture-conformance tests for the Kotlin port, written with [Konsist](https://github.com/LemonAppDev/konsist)
and Kotest. This is the analogue of the Go reference's `ARCH/` package and the Python port's
`arch/` tests. Run them with the dedicated command:

```bash
./gradlew arch        # = :konsist:test (separate from the unit-test run)
```

## Rules enforced (ports of `ARCH/arch_test.go` + `ARCH/docs_test.go`)

- **Tooling must not import agents.** Files in `githubapi`, `gitrepo`, `webhook`, `notify`,
  `scheduler`, `reconcile` may not import `io.github.jkjamies.automationagent.agent...`.
- **Provider SDKs only in `agent.setup`.** Ollama / ADK-Gemini / genai imports are confined
  to the `agent.setup` package (vacuously true until that layer is ported).
- **Nothing imports the entrypoint.** No file outside `app` imports the `app` package.
- **Every source package directory has an `AGENTS.md`.**

These are language-neutral architecture rules; see `../../.agents/standards/architecture.md`
and `.agents/standards/architecture-design.md` at the repo root.
