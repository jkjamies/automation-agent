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
	"net/url"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v78/github"
)

// IdentityResolver is optionally implemented by a TokenProvider that can report the GitHub login
// its tokens author content as ("<app-slug>[bot]" in App mode, the user login in PAT mode). It
// lets a caller attribute the service's own comments without guessing from author type. A
// provider that cannot resolve an identity (anonymous) returns "" with no error.
type IdentityResolver interface {
	AuthoredLogin(ctx context.Context) (string, error)
}

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

// AuthoredLogin resolves the authenticated user login for the PAT via GET /user, so the service
// can recognize comments it authored in PAT mode. An empty token yields "" (anonymous — there is
// no identity to attribute).
func (p StaticProvider) AuthoredLogin(ctx context.Context) (string, error) {
	if p.token == "" {
		return "", nil
	}
	gh := github.NewClient(nil).WithAuthToken(p.token)
	u, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("auth: resolve user identity: %w", err)
	}
	return u.GetLogin(), nil
}

// AppProvider mints and caches a short-lived installation token for a single
// pinned installation. ghinstallation's Transport handles the JWT minting, the
// token exchange, caching, and proactive refresh; AppProvider adapts it to the
// TokenProvider seam.
type AppProvider struct {
	tr   *ghinstallation.Transport
	apps *ghinstallation.AppsTransport // app-level (JWT) auth, for resolving the app's own identity
	// baseURL overrides the GitHub API base for tests (token exchange, JWT GET /app); empty in
	// production. loginMu guards login, which caches the resolved "<slug>[bot]" identity; only a
	// success is cached, so a transient lookup failure can be retried on a later call.
	baseURL string
	loginMu sync.Mutex
	login   string
}

// AppOption configures an AppProvider.
type AppOption func(*AppProvider)

// WithBaseURL overrides the GitHub API base used for the token-exchange call
// (POST /app/installations/{id}/access_tokens) and the JWT identity lookup (GET /app). Tests
// point this at an httptest stub; production leaves it at the default (https://api.github.com).
func WithBaseURL(rawURL string) AppOption {
	return func(p *AppProvider) { p.baseURL = rawURL }
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
	apps, err := ghinstallation.NewAppsTransport(base, appID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("auth: build apps transport: %w", err)
	}
	p := &AppProvider{tr: tr, apps: apps}
	for _, opt := range opts {
		opt(p)
	}
	if p.baseURL != "" {
		p.tr.BaseURL = p.baseURL
		p.apps.BaseURL = p.baseURL
	}
	return p, nil
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

// AuthoredLogin resolves the "<app-slug>[bot]" login this App authors content as, via a
// JWT-authenticated GET /app, so the service can recognize the comments it posted. The result is
// resolved once and cached (the slug is immutable for the deployment's lifetime).
func (p *AppProvider) AuthoredLogin(ctx context.Context) (string, error) {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	if p.login != "" {
		return p.login, nil
	}
	gh := github.NewClient(&http.Client{Transport: p.apps})
	if p.baseURL != "" {
		u, err := url.Parse(p.baseURL + "/")
		if err != nil {
			return "", fmt.Errorf("auth: parse base url: %w", err)
		}
		gh.BaseURL = u
	}
	app, _, err := gh.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("auth: resolve app identity: %w", err)
	}
	if app.GetSlug() == "" {
		return "", fmt.Errorf("auth: app identity response has no slug")
	}
	p.login = app.GetSlug() + "[bot]"
	return p.login, nil
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
