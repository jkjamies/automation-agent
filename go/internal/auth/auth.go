// Package auth abstracts how the service authenticates to GitHub behind a single
// seam, the TokenProvider, so the rest of the code never sees whether a token came
// from a static PAT or a freshly minted GitHub App installation token.
//
// Two providers implement the seam:
//
//   - StaticProvider — returns one constant token for every repo. Backs the PAT
//     local-dev fallback (GITHUB_TOKEN/GH_TOKEN/`gh auth token`) and the empty,
//     anonymous client used for public reads and tests.
//   - AppProvider — mints and caches a short-lived (~1h), auto-refreshed
//     installation token for a single pinned installation (single-org per
//     deployment; see specs/20260625-github-app-authentication.md §1). The repo
//     argument is accepted for the contract but ignored: one installation covers
//     every repo in the deployment.
//
// NewRoundTripper bridges the seam to the REST client by injecting the provider's
// token into every request; gitrepo calls Token directly per git operation.
// Deterministic tooling — no agent imports.
package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

// TokenProvider yields a valid GitHub token for operations on a given repo.
// PAT mode returns the same constant for every repo; App mode mints/caches a
// repo-scoped installation token and refreshes it before expiry. repo is
// "owner/name".
type TokenProvider interface {
	Token(ctx context.Context, repo string) (string, error)
}

// StaticProvider returns the same token for every repo. It backs PAT mode and the
// empty/anonymous client (an empty token means unauthenticated).
type StaticProvider struct{ token string }

// NewStaticProvider returns a provider that always yields token. An empty token
// is valid and yields an unauthenticated (public-read) client downstream.
func NewStaticProvider(token string) StaticProvider { return StaticProvider{token: token} }

// Token returns the constant token; repo and ctx are ignored.
func (p StaticProvider) Token(context.Context, string) (string, error) { return p.token, nil }

// AppProvider mints and caches a short-lived installation token for a single
// pinned installation. ghinstallation's Transport handles the JWT minting, the
// token exchange, caching, and proactive refresh; AppProvider adapts it to the
// TokenProvider seam.
type AppProvider struct {
	tr *ghinstallation.Transport
}

// AppOption configures an AppProvider.
type AppOption func(*ghinstallation.Transport)

// WithBaseURL overrides the GitHub API base used for the token-exchange call
// (POST /app/installations/{id}/access_tokens). Tests point this at an httptest
// stub; production leaves it at the default (https://api.github.com).
func WithBaseURL(url string) AppOption {
	return func(tr *ghinstallation.Transport) { tr.BaseURL = url }
}

// NewAppProvider builds an App provider pinned to one installation. privateKeyPEM
// is the App private key in PEM form (the caller is responsible for sourcing and
// validating it — see config). base is the underlying RoundTripper for the
// token-exchange call (nil → http.DefaultTransport).
func NewAppProvider(base http.RoundTripper, appID, installationID int64, privateKeyPEM []byte, opts ...AppOption) (*AppProvider, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	tr, err := ghinstallation.New(base, appID, installationID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("auth: build app transport: %w", err)
	}
	for _, opt := range opts {
		opt(tr)
	}
	return &AppProvider{tr: tr}, nil
}

// Token returns a currently-valid installation token, minting or refreshing it as
// needed. repo is ignored: the installation is pinned and covers every repo.
func (p *AppProvider) Token(ctx context.Context, _ string) (string, error) {
	tok, err := p.tr.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("auth: mint installation token: %w", err)
	}
	return tok, nil
}

// roundTripper injects a per-request bearer token from a TokenProvider. It backs
// the REST client in both modes: PAT (constant token) and App (cached, auto-
// refreshed installation token). An empty token is left unauthenticated.
type roundTripper struct {
	base http.RoundTripper
	p    TokenProvider
}

// NewRoundTripper wraps base so every request carries a fresh token from p. A nil
// base uses http.DefaultTransport; a nil provider degrades to an unauthenticated
// (anonymous) client rather than panicking on the first request. This is the bridge
// from the TokenProvider seam to the go-github REST client (githubapi).
func NewRoundTripper(base http.RoundTripper, p TokenProvider) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if p == nil {
		p = StaticProvider{} // anonymous: empty token, no Authorization header
	}
	return &roundTripper{base: base, p: p}
}

// RoundTrip fetches a token from the provider and, when non-empty, sets it as the
// Authorization bearer on a clone of the request (per the RoundTripper contract,
// the inbound request must not be mutated).
func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.p.Token(req.Context(), "")
	if err != nil {
		return nil, fmt.Errorf("auth: token for %s: %w", req.URL.Path, err)
	}
	if tok != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return t.base.RoundTrip(req)
}
