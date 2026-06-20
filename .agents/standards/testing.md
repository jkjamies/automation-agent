# Testing

- **Coverage ≥ 80%**, enforced by `make cover` (and `make ci`). Put the hard logic
  in injectable, LLM-free functions so it's reachable by unit tests.
- **Never assert on LLM output content.** LLM responses are non-deterministic;
  tests that check for keywords/tone/persona are flaky by nature. Validate agent
  *wiring* (with a fake `model.LLM`) and *deterministic tooling* instead. Behavior
  quality is checked manually / via eval, not pytest-style content assertions.
- **Test the build-agent pattern:** `Build<Name>Agent` is tested with fakes to
  assert structure; `<name>.go` logic is tested directly.
- **Tooling tests** use `httptest` stubs for GitHub, Slack/Teams, and Ollama — no
  real network in unit tests.
- **Table-driven tests** where it reduces duplication. Name tests for behavior.
- Keep tests in the same package for white-box access, or `_test` package when
  asserting the public API surface.
