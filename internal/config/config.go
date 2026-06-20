// Package config loads the automation-agent runtime configuration from the
// environment. It is the single source of truth for settings; no other package
// should read os.Getenv directly. See docs/architecture.md §12.
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

// Config holds all runtime settings.
type Config struct {
	// LLM
	LLMProvider Provider
	OllamaHost  string
	OllamaModel string
	GeminiModel string

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
	MaxIterations       int
	CITimeout           time.Duration
	ReconcileInterval   time.Duration
	GitHubWebhookSecret string
	AgentPRLabel        string
	AgentCheckName      string
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
		GeminiModel:         getOr(get, "GEMINI_MODEL", ""),
		Repos:               splitList(getOr(get, "REPOS", "")),
		GitHubToken:         getOr(get, "GITHUB_TOKEN", ""),
		NotifyProvider:      NotifyProvider(getOr(get, "NOTIFY_PROVIDER", string(NotifySlack))),
		SlackWebhookURL:     getOr(get, "SLACK_WEBHOOK_URL", ""),
		TeamsWebhookURL:     getOr(get, "TEAMS_WEBHOOK_URL", ""),
		Port:                getOr(get, "PORT", "8080"),
		CronDaily:           getOr(get, "CRON_DAILY", "0 9 * * *"),
		CronWeekly:          getOr(get, "CRON_WEEKLY", "0 9 * * 1"),
		GitHubWebhookSecret: getOr(get, "GITHUB_WEBHOOK_SECRET", ""),
		AgentPRLabel:        getOr(get, "AGENT_PR_LABEL", "automation-agent"),
		AgentCheckName:      getOr(get, "AGENT_CHECK_NAME", "agent-lint-verify"),
	}

	var err error
	if c.MaxIterations, err = strconv.Atoi(getOr(get, "MAX_ITERATIONS", "3")); err != nil {
		return Config{}, fmt.Errorf("MAX_ITERATIONS: %w", err)
	}
	if c.CITimeout, err = time.ParseDuration(getOr(get, "CI_TIMEOUT", "90m")); err != nil {
		return Config{}, fmt.Errorf("CI_TIMEOUT: %w", err)
	}
	if c.ReconcileInterval, err = time.ParseDuration(getOr(get, "RECONCILE_INTERVAL", "15m")); err != nil {
		return Config{}, fmt.Errorf("RECONCILE_INTERVAL: %w", err)
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
	if c.MaxIterations < 1 {
		return fmt.Errorf("MAX_ITERATIONS must be >= 1, got %d", c.MaxIterations)
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
