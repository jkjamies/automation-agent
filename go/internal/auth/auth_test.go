package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testKeyPEM generates a throwaway RSA private key in PKCS#1 PEM form (the shape of
// a real GitHub App key) so tests never touch a real secret.
func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestStaticProvider(t *testing.T) {
	p := NewStaticProvider("pat-123")
	tok, err := p.Token(context.Background(), "owner/repo")
	if err != nil || tok != "pat-123" {
		t.Fatalf("Token = (%q, %v), want (pat-123, nil)", tok, err)
	}
	// Empty token is valid (anonymous).
	empty, err := NewStaticProvider("").Token(context.Background(), "x/y")
	if err != nil || empty != "" {
		t.Fatalf("empty Token = (%q, %v), want (\"\", nil)", empty, err)
	}
}

// recordingRT records the last Authorization header it saw, then 200s.
type recordingRT struct{ auth string }

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.auth = req.Header.Get("Authorization")
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestRoundTripperInjectsBearer(t *testing.T) {
	base := &recordingRT{}
	rt := NewRoundTripper(base, NewStaticProvider("tok-abc"))
	req, _ := http.NewRequest("GET", "https://api.github.com/x", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.auth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", base.auth)
	}
	// The inbound request must not be mutated (RoundTripper contract).
	if req.Header.Get("Authorization") != "" {
		t.Errorf("inbound request was mutated: %q", req.Header.Get("Authorization"))
	}
}

func TestRoundTripperEmptyTokenUnauthenticated(t *testing.T) {
	base := &recordingRT{}
	rt := NewRoundTripper(base, NewStaticProvider(""))
	req, _ := http.NewRequest("GET", "https://api.github.com/x", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.auth != "" {
		t.Errorf("Authorization = %q, want empty (anonymous)", base.auth)
	}
}

// errProvider always fails, to assert the RoundTripper surfaces token errors.
type errProvider struct{}

func (errProvider) Token(context.Context, string) (string, error) {
	return "", errors.New("boom")
}

func TestRoundTripperPropagatesTokenError(t *testing.T) {
	rt := NewRoundTripper(&recordingRT{}, errProvider{})
	req, _ := http.NewRequest("GET", "https://api.github.com/x", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected the token error to propagate")
	}
}

// appJWTClaims decodes the unverified payload of the app JWT the exchange request
// carries, so a test can assert the issuer (App ID).
func appJWTClaims(t *testing.T, authHeader string) map[string]any {
	t.Helper()
	tok := strings.TrimPrefix(authHeader, "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("app JWT is not a 3-part token: %q", authHeader)
	}
	// Header: assert RS256.
	hdrRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode jwt header: %v", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrRaw, &hdr); err != nil {
		t.Fatalf("parse jwt header: %v", err)
	}
	if hdr["alg"] != "RS256" {
		t.Errorf("jwt alg = %v, want RS256", hdr["alg"])
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		t.Fatalf("parse jwt payload: %v", err)
	}
	return claims
}

func TestAppProviderMintExchangeAndCache(t *testing.T) {
	const appID, installID = int64(42), int64(99)
	var exchanges int32
	var sawAuth, sawPath string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&exchanges, 1)
		sawAuth = r.Header.Get("Authorization")
		sawPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installation_token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p, err := NewAppProvider(nil, appID, installID, testKeyPEM(t), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewAppProvider: %v", err)
	}

	tok, err := p.Token(context.Background(), "acme/api")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_installation_token" {
		t.Errorf("token = %q, want ghs_installation_token", tok)
	}
	// The exchange targets the pinned installation id (no dynamic resolution).
	if !strings.Contains(sawPath, "/app/installations/99/access_tokens") {
		t.Errorf("exchange path = %q, want pinned installation 99", sawPath)
	}
	// The exchange authenticates as the app via an RS256 JWT issued by the App ID.
	claims := appJWTClaims(t, sawAuth)
	// JSON numbers decode to float64; the App ID may be the issuer as a string or number.
	if iss, ok := claims["iss"].(float64); ok {
		if int64(iss) != appID {
			t.Errorf("jwt iss = %v, want %d", iss, appID)
		}
	} else if iss, ok := claims["iss"].(string); !ok || iss != "42" {
		t.Errorf("jwt iss = %v, want %d", claims["iss"], appID)
	}

	// Second call within validity reuses the cached token — no new exchange.
	if _, err := p.Token(context.Background(), "acme/api"); err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if got := atomic.LoadInt32(&exchanges); got != 1 {
		t.Errorf("exchanges = %d, want 1 (token should be cached)", got)
	}
}

// AuthoredLogin resolves the app's "<slug>[bot]" identity via a JWT-authenticated GET /app and
// caches it, so the service can recognize the comments it posted.
func TestAppProviderAuthoredLogin(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// /app authenticates as the app via an RS256 JWT bearer, not an installation token.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("GET /app missing bearer auth: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "slug": "agent-app"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p, err := NewAppProvider(nil, 42, 99, testKeyPEM(t), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewAppProvider: %v", err)
	}
	login, err := p.AuthoredLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthoredLogin: %v", err)
	}
	if login != "agent-app[bot]" {
		t.Errorf("login = %q, want agent-app[bot]", login)
	}
	// Resolved once and cached — a second call makes no further request.
	if _, err := p.AuthoredLogin(context.Background()); err != nil {
		t.Fatalf("AuthoredLogin (cached): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("GET /app calls = %d, want 1 (identity should be cached)", got)
	}
}

func TestAppProviderRefreshesExpiredToken(t *testing.T) {
	var exchanges int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&exchanges, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "tok-" + string(rune('0'+n)),
			// Already-expired (past) expiry forces a fresh exchange on each call.
			"expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p, err := NewAppProvider(nil, 1, 2, testKeyPEM(t), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewAppProvider: %v", err)
	}
	if _, err := p.Token(context.Background(), "a/b"); err != nil {
		t.Fatalf("Token #1: %v", err)
	}
	if _, err := p.Token(context.Background(), "a/b"); err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if got := atomic.LoadInt32(&exchanges); got < 2 {
		t.Errorf("exchanges = %d, want >= 2 (expired token must refresh)", got)
	}
}

func TestNewAppProviderRejectsInvalidKey(t *testing.T) {
	if _, err := NewAppProvider(nil, 1, 2, []byte("not a pem key")); err == nil {
		t.Fatal("expected an error for an invalid private key")
	}
}

// PAT mode resolves the service's own login via GET /user, which is what lets the reviewer
// recognize the comments it authored. An empty token is anonymous — there is no identity to
// attribute, and asking GitHub would only earn a 401.
func TestStaticProviderAuthoredLogin(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer srv.Close()

	login, err := NewStaticProvider("pat-123", WithStaticBaseURL(srv.URL)).AuthoredLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthoredLogin: %v", err)
	}
	if login != "octocat" {
		t.Errorf("login = %q, want octocat", login)
	}
	if !strings.Contains(gotAuth, "pat-123") {
		t.Errorf("identity lookup must authenticate as the PAT, got %q", gotAuth)
	}
}

func TestStaticProviderAuthoredLoginAnonymous(t *testing.T) {
	login, err := NewStaticProvider("").AuthoredLogin(context.Background())
	if err != nil || login != "" {
		t.Fatalf("AuthoredLogin = (%q, %v), want (\"\", nil) for an empty token", login, err)
	}
}

// A failed lookup is an error, not a silently empty login: the caller warns and falls back to
// author-type matching, and it can only make that choice if it is told.
func TestStaticProviderAuthoredLoginError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := NewStaticProvider("bad", WithStaticBaseURL(srv.URL)).AuthoredLogin(context.Background()); err == nil {
		t.Fatal("a rejected identity lookup must surface as an error")
	}
}

// The zero arguments are the ones a caller reaches for by accident. A nil base must not panic on
// the first request, and a nil provider must degrade to anonymous rather than dereference nil —
// the read-only flows in local dev depend on it.
func TestNewRoundTripperNilArguments(t *testing.T) {
	if rt := NewRoundTripper(nil, NewStaticProvider("")); rt == nil {
		t.Fatal("a nil base must fall back to the default transport")
	}
	rec := &recordingRT{}
	rt := NewRoundTripper(rec, nil)
	req, _ := http.NewRequest("GET", "https://api.github.com/x", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("nil provider should be anonymous, got %v", err)
	}
	if rec.auth != "" {
		t.Errorf("nil provider set an Authorization header %q, want none", rec.auth)
	}
}
