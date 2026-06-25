// Package config loads the automation-agent runtime configuration from the
// environment. It is the single source of truth for settings; no other package
// should read os.Getenv directly. See .agents/standards/architecture-design.md §12.
package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Provider selects which LLM backend agents use.
type Provider string

const (
	ProviderOllama Provider = "ollama"
	ProviderGemini Provider = "gemini"
)

// NotifyProvider selects where summaries are posted.
type NotifyProvider string

const (
	NotifySlack NotifyProvider = "slack"
	NotifyTeams NotifyProvider = "teams"
)

// SessionBackend selects where the ADK session (the durable suspend/resume history of
// the parked fix loop) is stored.
type SessionBackend string

const (
	// SessionMemory keeps sessions in-process: tests and ephemeral local runs. A restart
	// strands parked runs. This is the default — selecting it changes nothing.
	SessionMemory SessionBackend = "memory"
	// SessionSQLite persists sessions to a local file via the adk session/database
	// backend, so a parked run survives a restart. For real local runs.
	SessionSQLite SessionBackend = "sqlite"
	// SessionFirestore is the cloud backend (serverless, scales to zero): a custom
	// Firestore session.Service + ParkStore, both built under internal/agent/setup.
	SessionFirestore SessionBackend = "firestore"
)

// Config holds all runtime settings.
type Config struct {
	// LLM
	LLMProvider Provider
	OllamaHost  string
	OllamaModel string // default model: triage, explore, summary
	GeminiModel string
	// Code model: the (typically larger) model used for the code-change steps
	// (lint rewrite, coverage test generation). Falls back to the default model.
	OllamaCodeModel string
	GeminiCodeModel string

	// Sessions
	SessionBackend SessionBackend
	// SQLiteDSN is the data source for SESSION_BACKEND=sqlite (ignored otherwise). A
	// glebarez/modernc DSN: a file path, optionally with ?_pragma=… options.
	SQLiteDSN string
	// FirestoreProject is the GCP project for SESSION_BACKEND=firestore; empty detects it
	// from ADC / GOOGLE_CLOUD_PROJECT. FirestoreCollection is the collection-name prefix.
	FirestoreProject    string
	FirestoreCollection string

	// GitHub / repos
	Repos       []string
	GitHubToken string
	// GitTransport selects the git clone/push transport: "https" (default — uses GitHubToken)
	// or "ssh" (local dev — ssh-agent/keys). SSH only covers the git transport; the GitHub
	// REST API (open/label PR, read CI) still needs a token, so an ssh run without a token
	// warns at startup.
	GitTransport string
	// GitSSHKey is an explicit private-key path for GitTransport=ssh (GIT_SSH_KEY); empty
	// falls back to ssh-agent then the default identity files.
	GitSSHKey string

	// Notifications
	NotifyProvider  NotifyProvider
	SlackWebhookURL string
	TeamsWebhookURL string

	// Server
	Port string

	// Lint-fixer
	MaxIterations int
	// CITimeout bounds how long a suspended fix run waits for its CI result before
	// it is resumed with a timeout outcome (notify + stop). Per-run timer, not a scan.
	CITimeout           time.Duration
	GitHubWebhookSecret string
	// InternalToken is the Bearer token guarding the /internal/* endpoints (Cloud Scheduler
	// cron + sweep). Empty disables those endpoints (404).
	InternalToken string
	// AgentPRLabel is the single human-facing label applied to every agent PR on creation
	// (AGENT_PR_LABEL). Write-only: PR lookup is by branch, so the label never gates behavior.
	AgentPRLabel string
}

// Load reads configuration from the process environment, applying defaults.
func Load() (Config, error) {
	c, err := loadFrom(os.LookupEnv)
	if err != nil {
		return Config{}, err
	}
	// When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's gh
	// CLI login so a local run authenticates to GitHub without a hand-set token. Any
	// failure (gh absent, not logged in, timeout) leaves the token empty (anonymous).
	if c.GitHubToken == "" {
		c.GitHubToken = ghCLIToken()
	}
	return c, nil
}

// ghCLIToken returns the token from `gh auth token`, or "" if the gh CLI is missing,
// unauthenticated, or errors. This is the one place config shells out rather than
// reading the environment; it exists so local runs reuse an existing gh login. The
// short timeout guards against a hung subprocess stalling startup.
func ghCLIToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// loadFrom builds a Config from an arbitrary lookup func, which keeps Load
// testable without mutating the real environment.
func loadFrom(get lookup) (Config, error) {
	c := Config{
		LLMProvider:         Provider(getOr(get, "LLM_PROVIDER", string(ProviderOllama))),
		OllamaHost:          getOr(get, "OLLAMA_HOST", "http://localhost:11434"),
		OllamaModel:         getOr(get, "OLLAMA_MODEL", "gemma4:12b"),
		OllamaCodeModel:     getOr(get, "OLLAMA_CODE_MODEL", "gemma4:26b"),
		GeminiModel:         getOr(get, "GEMINI_MODEL", ""),
		GeminiCodeModel:     getOr(get, "GEMINI_CODE_MODEL", ""),
		SessionBackend:      SessionBackend(getOr(get, "SESSION_BACKEND", string(SessionMemory))),
		SQLiteDSN:           getOr(get, "SQLITE_DSN", "file:automation-agent.db?_pragma=busy_timeout(5000)"),
		FirestoreProject:    getOr(get, "FIRESTORE_PROJECT", ""),
		FirestoreCollection: getOr(get, "FIRESTORE_COLLECTION", "automation_agent"),
		Repos:               splitList(getOr(get, "REPOS", "")),
		GitHubToken:         getOr(get, "GITHUB_TOKEN", getOr(get, "GH_TOKEN", "")),
		GitTransport:        getOr(get, "GIT_TRANSPORT", "https"),
		GitSSHKey:           getOr(get, "GIT_SSH_KEY", ""),
		NotifyProvider:      NotifyProvider(getOr(get, "NOTIFY_PROVIDER", string(NotifySlack))),
		SlackWebhookURL:     getOr(get, "SLACK_WEBHOOK_URL", ""),
		TeamsWebhookURL:     getOr(get, "TEAMS_WEBHOOK_URL", ""),
		Port:                getOr(get, "PORT", "8080"),
		GitHubWebhookSecret: getOr(get, "GITHUB_WEBHOOK_SECRET", ""),
		InternalToken:       getOr(get, "INTERNAL_TOKEN", ""),
		AgentPRLabel:        getOr(get, "AGENT_PR_LABEL", "automation-agent"),
	}

	var err error
	if c.MaxIterations, err = strconv.Atoi(getOr(get, "MAX_ITERATIONS", "3")); err != nil {
		return Config{}, fmt.Errorf("MAX_ITERATIONS: %w", err)
	}
	if c.CITimeout, err = time.ParseDuration(getOr(get, "CI_TIMEOUT", "90m")); err != nil {
		return Config{}, fmt.Errorf("CI_TIMEOUT: %w", err)
	}

	// Code models default to the base models when unset.
	if c.OllamaCodeModel == "" {
		c.OllamaCodeModel = c.OllamaModel
	}
	if c.GeminiCodeModel == "" {
		c.GeminiCodeModel = c.GeminiModel
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks invariants that defaults alone cannot guarantee.
func (c Config) Validate() error {
	switch c.LLMProvider {
	case ProviderOllama, ProviderGemini:
	default:
		return fmt.Errorf("invalid LLM_PROVIDER %q (want ollama|gemini)", c.LLMProvider)
	}
	switch c.NotifyProvider {
	case NotifySlack, NotifyTeams:
	default:
		return fmt.Errorf("invalid NOTIFY_PROVIDER %q (want slack|teams)", c.NotifyProvider)
	}
	switch c.SessionBackend {
	case SessionMemory, SessionSQLite, SessionFirestore:
	default:
		return fmt.Errorf("invalid SESSION_BACKEND %q (want memory|sqlite|firestore)", c.SessionBackend)
	}
	switch c.GitTransport {
	case "https", "ssh":
	default:
		return fmt.Errorf("invalid GIT_TRANSPORT %q (want https|ssh)", c.GitTransport)
	}
	if c.MaxIterations < 1 {
		return fmt.Errorf("MAX_ITERATIONS must be >= 1, got %d", c.MaxIterations)
	}
	if c.CITimeout <= 0 {
		return fmt.Errorf("CI_TIMEOUT must be > 0, got %s", c.CITimeout)
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil {
		return fmt.Errorf("PORT must be numeric, got %q", c.Port)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("PORT must be in 1..65535, got %d", port)
	}
	return nil
}

type lookup func(string) (string, bool)

// getOr returns the trimmed value for key, or def when unset or blank. Trimming
// guards against trailing whitespace/newlines on values from the real environment
// (e.g. a CI secret with a trailing newline); godotenv already trims values it parses.
func getOr(get lookup, key, def string) string {
	if v, ok := get(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
