# githubapi

A thin wrapper over the GitHub REST API with the narrow operations this service needs:
recent commits, open/label/find agent PRs, attempt count, the agent verify check, file
contents, and parsing `check_run` webhook events. Deterministic tooling — **no agent imports**.

## Details

- `GitHubApi.kt` — `Client` (suspend methods) built on the **Ktor client** (CIO engine) with
  content negotiation + `kotlinx.serialization`. The REST endpoints are hit directly to keep
  deps light, and the client is pointed at an injectable `baseUrl`/engine for testing. An
  `HttpClient` is injectable for tests.
- Pagination follows the `Link: …; rel="next"` header.
- Auth comes from an injectable `TokenSource` (the githubapi-local view of the `auth.TokenProvider`
  seam). A request-pipeline interceptor calls it **per request** and sets `Authorization: Bearer …`
  when the token is non-empty — the analogue of the Go reference's token-injecting `RoundTripper`, so
  a short-lived App installation token stays current across a long run. A null source (or one yielding
  `""`) leaves requests unauthenticated.
- `Client.parseCheckRunEvent(body)` is a pure parse (companion function).
- Public projections (`Commit`, `Pr`, `PrInput`, `CheckResult`, `CheckEvent`) carry only the
  fields this service uses. `Commit.at` is the author date.

Tested against a Ktor `MockEngine` that routes by method+path — no real GitHub calls.
