# auth

The GitHub authentication seam. One interface, `TokenProvider`, hides whether a token comes from a
static PAT (local-dev fallback) or a freshly minted GitHub App installation token (production). See
`specs/20260625-github-app-authentication.md`.

## Flow

```mermaid
flowchart TD
    CFG[config: mode selection] -->|App vars set| AP["newAppProvider(appId, installId, pem)"]
    CFG -->|else PAT / empty| SP["StaticProvider(token)"]

    AP -->|"parseRsaPrivateKey(pem)"| KEY["RSAPrivateKey"]
    KEY --> APP["AppProvider"]
    SP --> STAT["StaticProvider"]

    APP -->|"token(repo) — repo ignored, pinned install"| TP{{TokenProvider}}
    STAT -->|"token(repo) — constant"| TP

    APP -.->|first call / near expiry| MINT["buildAppJwt (RS256) → POST /app/installations/&#123;id&#125;/access_tokens"]
    MINT -.->|"cache token + expires_at"| APP

    TP -->|REST| GHC["githubapi.Client(tokenSource)"]
    GHC -->|"per request: Authorization: Bearer token()"| GH[(GitHub REST API)]
    TP -->|git clone/push| GR["gitrepo: x-access-token = token(repo) per op"]
    GR --> GH
```

- `TokenProvider.token(repo)` — the seam. `repo` is `"owner/name"`. PAT mode returns the same
  constant for every repo; App mode mints/caches a short-lived (~1h) installation token and refreshes
  it before expiry. The seam is the **cross-port contract** (`language-parity.md`); the minting
  mechanism is per-port detail.
- `StaticProvider` — constant token. Backs the PAT fallback and the empty (anonymous,
  public-read/test) client. An empty token is valid.
- `AppProvider` — pinned to **one** installation id (single-org per deployment — spec §1), so there
  is no per-owner cache and no dynamic `repo→installation` resolution. The `repo` argument is accepted
  for the contract but ignored. The JVM has no off-the-shelf installation-token library, so the App
  flow is hand-rolled: `buildAppJwt` signs an RS256 JWT with the App key (`java.security`), `exchange`
  trades it at `POST /app/installations/{id}/access_tokens` for an installation token, and `token`
  caches that token, re-minting ~1 min before `expires_at`. A mutex serializes concurrent callers so
  the token is minted at most once per refresh window. `baseUrl` / `httpClient` / `now` are injectable
  for tests.
- `parseRsaPrivateKey` — parses the App private key PEM, accepting both **PKCS#1**
  (`-----BEGIN RSA PRIVATE KEY-----`, the shape GitHub hands out) and **PKCS#8**
  (`-----BEGIN PRIVATE KEY-----`) via Bouncy Castle, and rejecting a non-RSA key. The JDK's
  `KeyFactory` reads only PKCS#8, hence Bouncy Castle (the JVM analogue of Go's `x509`, which parses
  both natively). Mode selection, the env vars, and the `\n`-unescape live in `config`, which calls
  this for fail-fast validation; this package re-parses the same PEM for signing.

The REST client (`githubapi.Client`) takes a token source and injects `Authorization: Bearer <token>`
per request (the analogue of the Go reference's token-injecting `RoundTripper`); `gitrepo` calls
`token` directly per git operation, so a short-lived installation token stays current across a long
run. Deterministic tooling — no agent imports. Tested with a throwaway RSA key + a Ktor `MockEngine`
stub for the token exchange (no live network, no LLM).
