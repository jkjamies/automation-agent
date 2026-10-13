// Package config loads ticketflow's settings. It is the only package that reads the
// process environment; everything else receives values through Config.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration.
type Config struct {
	// Jira.
	JiraBaseURL       string            // https://your-org.atlassian.net
	JiraEmail         string            // basic-auth user for Jira Cloud API tokens; empty means bearer auth
	JiraToken         string            // API token (Cloud) or personal access token (Data Center)
	JiraWebhookSecret string            // verifies inbound webhooks; empty disables the webhook route
	TriggerLabel      string            // the label that hands a ticket to the agent
	ClaimLabel        string            // set while a run owns the ticket
	NeedsHumanLabel   string            // set when a run gives up
	ReviewTransition  string            // Jira transition applied once the PR is open; empty skips it
	ProjectRepos      map[string]string // Jira project key -> owner/repo
	AllowedRepos      []string          // every repo a run may touch (also gates repo:<owner/name> labels)

	// GitHub.
	GitHubToken  string // installation or fine-grained token: contents + pull_requests write
	GitHubAPI    string // REST base, https://api.github.com
	GitHubHost   string // clone host, https://github.com
	BaseBranch   string // empty means the repository's default branch
	BranchPrefix string // agent branches are <prefix><KEY>-<slug>
	PRLabel      string // label added to every agent PR
	DraftPRs     bool   // open PRs as drafts
	BotName      string // commit author
	BotEmail     string

	// Models on Vertex AI.
	GCPProject    string
	GCPLocation   string
	PlannerModel  string
	CoderModel    string
	ReviewerModel string

	// Harness.
	Harness        string   // "adk" (in-process coding agent) or "cli" (external headless agent)
	HarnessCommand []string // argv for the cli harness; {prompt_file} and {workdir} are substituted
	HarnessTimeout time.Duration
	HarnessEnv     []string // KEY=VALUE pairs passed to the cli harness (names from TICKETFLOW_HARNESS_ENV)

	// Budgets.
	MaxAttempts     int // implement -> verify cycles before giving up
	MaxRevisions    int // self-review -> implement cycles before publishing with open findings
	MaxFiles        int // diff policy: changed files
	MaxChangedLines int // diff policy: added + deleted lines
	CheckTimeout    time.Duration

	// Process.
	WorkRoot string // parent directory for per-run checkouts
	Addr     string // HTTP listen address for `serve`
	Workers  int    // concurrent runs in `serve`
}

// FromEnv loads the configuration from the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

// Load builds a Config from getenv, applying defaults. It validates only what every
// mode needs; mode-specific requirements are checked by the caller that needs them.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var errs []error
	num := func(key string, def int) int {
		raw := get(key, "")
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s: want a non-negative integer, got %q", key, raw))
			return def
		}
		return n
	}
	dur := func(key string, def time.Duration) time.Duration {
		raw := get(key, "")
		if raw == "" {
			return def
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive duration, got %q", key, raw))
			return def
		}
		return d
	}
	boolean := func(key string, def bool) bool {
		raw := get(key, "")
		if raw == "" {
			return def
		}
		b, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want a boolean, got %q", key, raw))
			return def
		}
		return b
	}

	projectRepos, err := parseMap(get("TICKETFLOW_PROJECT_REPOS", ""))
	if err != nil {
		errs = append(errs, fmt.Errorf("TICKETFLOW_PROJECT_REPOS: %w", err))
	}

	c := Config{
		JiraBaseURL:       strings.TrimRight(get("JIRA_BASE_URL", ""), "/"),
		JiraEmail:         get("JIRA_EMAIL", ""),
		JiraToken:         get("JIRA_API_TOKEN", ""),
		JiraWebhookSecret: get("JIRA_WEBHOOK_SECRET", ""),
		TriggerLabel:      get("TICKETFLOW_TRIGGER_LABEL", "agent-ready"),
		ClaimLabel:        get("TICKETFLOW_CLAIM_LABEL", "agent-in-progress"),
		NeedsHumanLabel:   get("TICKETFLOW_NEEDS_HUMAN_LABEL", "agent-needs-human"),
		ReviewTransition:  get("TICKETFLOW_REVIEW_TRANSITION", ""),
		ProjectRepos:      projectRepos,
		AllowedRepos:      splitList(get("TICKETFLOW_ALLOWED_REPOS", "")),

		GitHubToken:  get("GITHUB_TOKEN", ""),
		GitHubAPI:    strings.TrimRight(get("GITHUB_API_URL", "https://api.github.com"), "/"),
		GitHubHost:   strings.TrimRight(get("GITHUB_HOST_URL", "https://github.com"), "/"),
		BaseBranch:   get("TICKETFLOW_BASE_BRANCH", ""),
		BranchPrefix: get("TICKETFLOW_BRANCH_PREFIX", "ticketflow/"),
		PRLabel:      get("TICKETFLOW_PR_LABEL", "agent-authored"),
		DraftPRs:     boolean("TICKETFLOW_DRAFT_PRS", true),
		BotName:      get("TICKETFLOW_BOT_NAME", "ticketflow"),
		BotEmail:     get("TICKETFLOW_BOT_EMAIL", "ticketflow@users.noreply.github.com"),

		GCPProject:    get("GOOGLE_CLOUD_PROJECT", ""),
		GCPLocation:   get("GOOGLE_CLOUD_LOCATION", "global"),
		PlannerModel:  get("TICKETFLOW_PLANNER_MODEL", DefaultReasoningModel),
		CoderModel:    get("TICKETFLOW_CODER_MODEL", DefaultReasoningModel),
		ReviewerModel: get("TICKETFLOW_REVIEWER_MODEL", DefaultReasoningModel),

		Harness:        get("TICKETFLOW_HARNESS", "adk"),
		HarnessCommand: strings.Fields(get("TICKETFLOW_HARNESS_COMMAND", "")),
		HarnessTimeout: dur("TICKETFLOW_HARNESS_TIMEOUT", 45*time.Minute),
		HarnessEnv:     passthrough(getenv, splitList(get("TICKETFLOW_HARNESS_ENV", ""))),

		MaxAttempts:     num("TICKETFLOW_MAX_ATTEMPTS", 3),
		MaxRevisions:    num("TICKETFLOW_MAX_REVISIONS", 1),
		MaxFiles:        num("TICKETFLOW_MAX_FILES", 25),
		MaxChangedLines: num("TICKETFLOW_MAX_CHANGED_LINES", 1500),
		CheckTimeout:    dur("TICKETFLOW_CHECK_TIMEOUT", 20*time.Minute),

		WorkRoot: get("TICKETFLOW_WORK_ROOT", os.TempDir()),
		Addr:     get("TICKETFLOW_ADDR", ":8080"),
		Workers:  num("TICKETFLOW_WORKERS", 2),
	}

	switch c.Harness {
	case "adk":
	case "cli":
		if len(c.HarnessCommand) == 0 {
			errs = append(errs, errors.New("TICKETFLOW_HARNESS=cli needs TICKETFLOW_HARNESS_COMMAND"))
		}
	default:
		errs = append(errs, fmt.Errorf("TICKETFLOW_HARNESS: want adk or cli, got %q", c.Harness))
	}
	if c.MaxAttempts == 0 {
		errs = append(errs, errors.New("TICKETFLOW_MAX_ATTEMPTS must be at least 1"))
	}
	if c.Workers == 0 {
		c.Workers = 1
	}
	return c, errors.Join(errs...)
}

// DefaultReasoningModel is the Vertex AI model used for every LLM role unless overridden.
// Model IDs move faster than code, so this is the one place the default lives.
const DefaultReasoningModel = "gemini-2.5-pro"

// RequireRun reports what `run` and `serve` need beyond the defaults: Jira and GitHub
// credentials, a repo mapping, and a Vertex AI project for the in-process LLM roles.
func (c Config) RequireRun() error {
	var errs []error
	if c.JiraBaseURL == "" || c.JiraToken == "" {
		errs = append(errs, errors.New("JIRA_BASE_URL and JIRA_API_TOKEN are required"))
	}
	if c.GitHubToken == "" {
		errs = append(errs, errors.New("GITHUB_TOKEN is required"))
	}
	if len(c.ProjectRepos) == 0 {
		errs = append(errs, errors.New("TICKETFLOW_PROJECT_REPOS must map at least one Jira project to a repo"))
	}
	if c.GCPProject == "" {
		errs = append(errs, errors.New("GOOGLE_CLOUD_PROJECT is required (models run on Vertex AI)"))
	}
	return errors.Join(errs...)
}

// RepoAllowed reports whether repo may be touched. Every mapped project repo is
// implicitly allowed; AllowedRepos adds more.
func (c Config) RepoAllowed(repo string) bool {
	for _, r := range c.ProjectRepos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	for _, r := range c.AllowedRepos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// parseMap parses "PROJ=owner/repo,OTHER=owner/other".
func parseMap(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range splitList(raw) {
		k, v, ok := strings.Cut(item, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || !strings.Contains(v, "/") {
			return nil, fmt.Errorf("want KEY=owner/repo, got %q", item)
		}
		out[strings.ToUpper(k)] = v
	}
	return out, nil
}

// passthrough resolves environment variable names to KEY=VALUE pairs, skipping unset ones.
// The cli harness runs with a scrubbed environment; these are the only variables it sees
// beyond PATH and HOME, so model credentials reach it without repository tokens doing so.
func passthrough(getenv func(string) string, names []string) []string {
	var out []string
	for _, n := range names {
		if v := getenv(n); v != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

func splitList(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
