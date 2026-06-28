package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPEM returns a throwaway RSA private key in PKCS#1 PEM form (the shape of a
// real GitHub App key) so config tests never touch a real secret.
func testPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// ecKeyPEM returns an ECDSA private key in PKCS#8 PEM form — a valid PEM key that is
// NOT RSA, used to assert the loader rejects non-RSA keys (RS256 needs RSA).
func ecKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ec key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// appEnv is the minimal valid App-mode environment (REPOS required by Decision §3).
func appEnv(t *testing.T, extra map[string]string) map[string]string {
	m := map[string]string{
		"GITHUB_APP_ID":              "42",
		"GITHUB_APP_INSTALLATION_ID": "99",
		"GITHUB_APP_PRIVATE_KEY":     testPEM(t),
		"REPOS":                      "acme/api",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestPATModeWhenNoAppVars(t *testing.T) {
	c, err := loadFrom(mapLookup(map[string]string{"GITHUB_TOKEN": "pat"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.AppMode() {
		t.Error("AppMode() = true with no App vars, want false (PAT mode)")
	}
	if len(c.GitHubApp.PrivateKeyPEM) != 0 || c.GitHubApp.AppID != 0 {
		t.Errorf("GitHubApp should be zero in PAT mode, got %+v", c.GitHubApp)
	}
}

func TestAppModeFullConfig(t *testing.T) {
	c, err := loadFrom(mapLookup(appEnv(t, nil)))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !c.AppMode() {
		t.Fatal("AppMode() = false, want true")
	}
	if c.GitHubApp.AppID != 42 || c.GitHubApp.InstallationID != 99 {
		t.Errorf("ids = %d/%d, want 42/99", c.GitHubApp.AppID, c.GitHubApp.InstallationID)
	}
	if len(c.GitHubApp.PrivateKeyPEM) == 0 {
		t.Error("PrivateKeyPEM is empty")
	}
}

func TestAppModeViaKeyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(testPEM(t)), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	env := map[string]string{
		"GITHUB_APP_ID":               "42",
		"GITHUB_APP_INSTALLATION_ID":  "99",
		"GITHUB_APP_PRIVATE_KEY_PATH": path,
		"REPOS":                       "acme/api",
	}
	c, err := loadFrom(mapLookup(env))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !c.AppMode() || len(c.GitHubApp.PrivateKeyPEM) == 0 {
		t.Errorf("App mode via key path failed: AppMode=%v keyLen=%d", c.AppMode(), len(c.GitHubApp.PrivateKeyPEM))
	}
}

func TestAppModeFlattenedKeyIsUnescaped(t *testing.T) {
	// CI secret stores often flatten newlines to the literal characters `\n`.
	flattened := strings.ReplaceAll(testPEM(t), "\n", `\n`)
	c, err := loadFrom(mapLookup(appEnv(t, map[string]string{"GITHUB_APP_PRIVATE_KEY": flattened})))
	if err != nil {
		t.Fatalf("loadFrom with flattened key: %v", err)
	}
	if !c.AppMode() {
		t.Fatal("AppMode() = false after unescaping a flattened key")
	}
	if !strings.Contains(string(c.GitHubApp.PrivateKeyPEM), "\n") {
		t.Error("PrivateKeyPEM still has no real newlines after unescaping")
	}
}

func TestAppModeFlattenedKeyWithTrailingNewlineIsUnescaped(t *testing.T) {
	// A secret store can flatten newlines to literal `\n` and still append one real
	// trailing newline; the unescape must run on the escaped sequences regardless.
	flattened := strings.ReplaceAll(testPEM(t), "\n", `\n`) + "\n"
	c, err := loadFrom(mapLookup(appEnv(t, map[string]string{"GITHUB_APP_PRIVATE_KEY": flattened})))
	if err != nil {
		t.Fatalf("loadFrom with flattened key + trailing newline: %v", err)
	}
	if !c.AppMode() {
		t.Fatal("AppMode() = false after unescaping a flattened key with a trailing newline")
	}
	if strings.Contains(string(c.GitHubApp.PrivateKeyPEM), `\n`) {
		t.Error("PrivateKeyPEM still contains escaped \\n after unescaping")
	}
}

func TestAppModeErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing installation": {"GITHUB_APP_ID": "42", "GITHUB_APP_PRIVATE_KEY": testPEM(t), "REPOS": "a/b"},
		"missing key":          {"GITHUB_APP_ID": "42", "GITHUB_APP_INSTALLATION_ID": "99", "REPOS": "a/b"},
		"both keys": {
			"GITHUB_APP_ID": "42", "GITHUB_APP_INSTALLATION_ID": "99",
			"GITHUB_APP_PRIVATE_KEY": testPEM(t), "GITHUB_APP_PRIVATE_KEY_PATH": "/tmp/x.pem", "REPOS": "a/b",
		},
		"missing app id":     {"GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": testPEM(t), "REPOS": "a/b"},
		"non-numeric app id": {"GITHUB_APP_ID": "abc", "GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": testPEM(t), "REPOS": "a/b"},
		"zero app id":        {"GITHUB_APP_ID": "0", "GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": testPEM(t), "REPOS": "a/b"},
		"negative app id":    {"GITHUB_APP_ID": "-1", "GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": testPEM(t), "REPOS": "a/b"},
		"zero installation":  {"GITHUB_APP_ID": "42", "GITHUB_APP_INSTALLATION_ID": "0", "GITHUB_APP_PRIVATE_KEY": testPEM(t), "REPOS": "a/b"},
		"invalid pem":        {"GITHUB_APP_ID": "42", "GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": "not-a-key", "REPOS": "a/b"},
		"non-rsa key":        {"GITHUB_APP_ID": "42", "GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": ecKeyPEM(t), "REPOS": "a/b"},
		"empty repos in app mode": {
			"GITHUB_APP_ID": "42", "GITHUB_APP_INSTALLATION_ID": "99", "GITHUB_APP_PRIVATE_KEY": testPEM(t),
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadFrom(mapLookup(env)); err == nil {
				t.Errorf("loadFrom(%s) = nil error, want a startup error", name)
			}
		})
	}
}

func TestStringRedactsSecrets(t *testing.T) {
	c, err := loadFrom(mapLookup(appEnv(t, map[string]string{
		"GITHUB_TOKEN":          "ghp_supersecretpat",
		"GITHUB_WEBHOOK_SECRET": "webhook-shhh",
		"INTERNAL_TOKEN":        "internal-shhh",
		"SLACK_WEBHOOK_URL":     "https://hooks.slack.com/services/SECRETPATH",
	})))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	// Every common formatting path must route through the redacting String methods.
	rendered := []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), c.String(), c.GitHubApp.String()}
	leaks := []string{"ghp_supersecretpat", "webhook-shhh", "internal-shhh", "SECRETPATH", "PRIVATE KEY"}
	for _, s := range rendered {
		for _, leak := range leaks {
			if strings.Contains(s, leak) {
				t.Errorf("formatted config leaked %q:\n%s", leak, s)
			}
		}
		if !strings.Contains(s, "***") {
			t.Errorf("formatted config has no masked marker:\n%s", s)
		}
	}
	// The raw private-key bytes (as printed by %v on a []byte) must not appear either.
	rawKeyBytes := fmt.Sprintf("%v", c.GitHubApp.PrivateKeyPEM)
	if strings.Contains(fmt.Sprintf("%+v", c), rawKeyBytes) {
		t.Errorf("formatted config leaked the raw private-key bytes")
	}
}

func TestPATModeEmptyReposAllowed(t *testing.T) {
	// PAT mode keeps "empty REPOS = all repos" for local-dev back-compat.
	c, err := loadFrom(mapLookup(map[string]string{"GITHUB_TOKEN": "pat"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if len(c.Repos) != 0 || c.AppMode() {
		t.Errorf("want PAT mode with empty repos, got AppMode=%v repos=%v", c.AppMode(), c.Repos)
	}
}
