# src/testutil

Test-only support code shared across suites. Not part of the runtime; it is
excluded from coverage and from the arch import-boundary checks (it is allowed to
import the ADK/provider types a real model adapter would).

- `fakes.ts` — `FakeLlm`, a deterministic `BaseLlm` that yields scripted text and
  records the requests it received. Lets agent wiring and logic be tested without a real
  model. Never assert on real LLM output content.
