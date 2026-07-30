---
type: Platform-Package
title: Egress identity
description: The single User-Agent every outbound request carries, and the seam each SDK exposes for it — so traffic from this service is legible in someone else's logs.
resource: go/internal/useragent
tags: [egress, user-agent, http, observability]
sensitivity: internal
bundle: automation-agent
timestamp: 2026-07-29T00:00:00Z
---

# Egress identity

Every request leaving this service identifies it. The default does not: `net/http` sends
`Go-http-client/1.1`, which says nothing about who is calling, and each vendored SDK sends its
own library name, which says nothing about the deployment. When a repository owner, a Slack
admin, or a GCP project owner looks at their logs and asks *what is this traffic*, the answer
should be readable without correlating timestamps.

`useragent.String()` is that answer: `automation-agent/<version>`, with the version read from
the module's own build info — no `ldflags`, and it cannot drift from what was compiled. An
untagged build reports `devel`.

The version is filtered to the characters RFC 9110 allows in a token, and that is load-bearing
rather than defensive. `go` reports an untagged build as `(devel)`, and parentheses are
delimiters that open a comment — `automation-agent/(devel)` parses as a product with **no
version** followed by a comment, which is not what it looks like. Stripping rather than
substituting leaves real versions untouched, pseudo-versions like
`v0.0.0-20260101120000-abcdef123456` included.

## Ours is added, never substituted

A library that sets its own `User-Agent` is telling the server something true about how the
request was built — go-github's REST version, ollama's client build, go-git's protocol support —
and a server may key behaviour off it. So ours is **prepended** and the rest survives.
Space-separated product tokens are exactly what a `User-Agent` is defined to be:

```text
automation-agent/devel go-github/v78.0.0
automation-agent/devel ollama/0.0.0 (amd64 linux) Go/go1.26.0
```

## The seam differs per destination

`useragent.Transport` is the common case — a `RoundTripper` decorator, so it applies to anything
built on an `http.Client` we construct. Where that is not available the SDK's own hook is used
instead, which is why this is a table rather than one wrapper:

| Egress | Seam |
|---|---|
| GitHub REST + GraphQL ([githubapi](/modules/platform/githubapi.md)) | `useragent.Transport`, outermost so it applies once per attempt and the rate-limit replay does not re-append |
| GitHub App token mint + identity lookup ([auth](/modules/platform/auth.md)) | `useragent.Transport` around the **base**, not the clients built on it — see below |
| Slack / Teams webhooks ([notify](/modules/platform/notify.md)) | header set on the request — callers may inject their own client, and the admin should see us either way |
| Ollama generation + preflight ([setup](/modules/agents/setup.md)) | `useragent.Transport` |
| Gemini / Vertex | `genai.ClientConfig.HTTPClient` — supplied *only* to brand the traffic, so the SDK's own timeout and retry behaviour are untouched |
| Cloud Tasks ([tasks](/modules/platform/tasks.md)) | `option.WithUserAgent` — gRPC, so there is no `RoundTripper` to wrap |
| OTLP trace export ([obs](/modules/platform/obs.md)) | merged into `WithHeaders`, because that option replaces the map wholesale and two calls would drop the operator's values |
| Cloud Trace export ([obs](/modules/platform/obs.md)) | `texporter.WithTraceClientOptions` |
| git clone / push ([gitrepo](/modules/platform/gitrepo.md)) | `GO_GIT_USER_AGENT_EXTRA`, go-git's documented hook — see below |

**The App provider brands its base, not its clients.** `ghinstallation` refreshes the
installation token by POSTing through its `AppsTransport`, which is the base handed to
`NewAppProvider` — so wrapping a client built on top would leave the token exchange, the most
frequent request the provider makes, anonymous. The base is wrapped once before either transport
is constructed, and both inherit it.

**git is the one that is not a client we own.** go-git builds its own HTTP requests, so there is
no `RoundTripper` to wrap; the library appends this environment variable to its own agent
string. It is set in `cmd/agent` rather than in `gitrepo` because writing the environment is
composition's business, not a tooling package's, and it must happen before any clone. An
operator-supplied value is kept and ours appended to it.

**Where an operator's value wins.** The OTLP header and the go-git variable are both settable
from outside, and in both cases an explicit setting is preserved — ours is the default, not an
override. For OTLP that requires canonicalizing the parsed header keys: the exporter calls
`Header.Set` per entry and `Set` canonicalizes, so `user-agent=…` and `User-Agent=…` are one
header. Left as distinct map keys they would collide only at send time, in map-iteration order,
making the winner vary per process. Everywhere else the value is not configurable, deliberately: a `User-Agent` is not a
place for a project id or a hostname, and an identifier that can be turned off is not one you
can rely on when reading someone else's logs.

## Testing

The package itself is tested directly — the token's shape, that every character of the version
is one RFC 9110 permits (checking only for whitespace is what let `(devel)` through), that it is
stable across calls, that an existing agent is preserved, and that the
caller's request is never mutated (the `RoundTripper` contract, and the reason a rate-limit
replay cannot accumulate our token once per attempt).

The two highest-traffic destinations, GitHub and the chat webhooks, additionally assert **at the
wire** — a stub server reads the header it actually received. What a header-setting call did is
not the thing that matters; what arrives is.
