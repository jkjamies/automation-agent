// Package config loads the automation-agent runtime configuration from the
// environment. It is the single source of truth for settings; no other package
// should read os.Getenv directly. See .agents/standards/architecture-design.md §12.
package config

import (
	"fmt"
	"os"
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
	// SessionFirestore is the cloud backend (scales to zero). Its custom session.Service
	// lands in Phase B; selecting it before then returns a not-implemented error.
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

	// GitHub / repos
	Repos       []string
	GitHubToken string

	// Notifications
	NotifyProvider  NotifyProvider
	SlackWebhookURL string
	TeamsWebhookURL string

	// Server / schedule
	Port       string
	CronDaily  string
	CronWeekly string

	// Lint-fixer
	MaxIterations int
	// CITimeout bounds how long a suspended fix run waits for its CI result before
	// it is resumed with a timeout outcome (notify + stop). Per-run timer, not a scan.
	CITimeout           time.Duration
	GitHubWebhookSecret string
}

// Load reads configuration from the process environment, applying defaults.
func Load() (Config, error) {
	return loadFrom(os.LookupEnv)
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
		Repos:               splitList(getOr(get, "REPOS", "")),
		GitHubToken:         getOr(get, "GITHUB_TOKEN", ""),
		NotifyProvider:      NotifyProvider(getOr(get, "NOTIFY_PROVIDER", string(NotifySlack))),
		SlackWebhookURL:     getOr(get, "SLACK_WEBHOOK_URL", ""),
		TeamsWebhookURL:     getOr(get, "TEAMS_WEBHOOK_URL", ""),
		Port:                getOr(get, "PORT", "8080"),
		CronDaily:           getOr(get, "CRON_DAILY", "0 9 * * *"),
		CronWeekly:          getOr(get, "CRON_WEEKLY", "0 9 * * 1"),
		GitHubWebhookSecret: getOr(get, "GITHUB_WEBHOOK_SECRET", ""),
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
	if c.MaxIterations < 1 {
		return fmt.Errorf("MAX_ITERATIONS must be >= 1, got %d", c.MaxIterations)
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

func getOr(get lookup, key, def string) string {
	if v, ok := get(key); ok && v != "" {
		return v
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
