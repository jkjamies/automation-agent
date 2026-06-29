// Package config loads the automation-agent runtime configuration from the
// environment. It is the single source of truth for settings; no other package
// should read os.Getenv directly. See .agents/standards/architecture-design.md §12.
package config

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
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

// TasksBackend selects the webhook execution transport: how an enqueued envelope reaches
// the dispatcher. See specs/20260626-workflow-execution-transport.md.
type TasksBackend string

const (
	// TasksInProcess runs each dispatch in a background goroutine pool (the pre-transport
	// behavior). The default — selecting it changes nothing. Local dev only: it does not
	// survive an instance being reclaimed mid-run, and on Cloud Run the compute is throttled
	// once the response is sent.
	TasksInProcess TasksBackend = "inprocess"
	// TasksCloudTasks enqueues each envelope as a Cloud Tasks HTTP-target task pointed at
	// /internal/dispatch, which executes it in-request (CPU stays allocated) with durable
	// retry + queue rate limiting. The production backend.
	TasksCloudTasks TasksBackend = "cloudtasks"
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
	// GitHubApp carries the resolved GitHub App credentials. A zero value
	// (AppID == 0) means App mode is off and the static GitHubToken (PAT) is used.
	// See AppMode and specs/20260625-github-app-authentication.md.
	GitHubApp GitHubApp
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

	// Reviewer (PR code-review agent). ReviewEnabled (REVIEW_ENABLED) is the kill switch:
	// false (the default) means pull_request events are accepted and acknowledged but no
	// review work runs. See specs/20260625-pr-code-review-agent.md.
	ReviewEnabled bool
	// ReviewSkipDrafts skips draft PRs unless the triggering action is ready_for_review
	// (REVIEW_SKIP_DRAFTS, default true).
	ReviewSkipDrafts bool
	// ReviewExcludeGlobs drops generated/vendored/lockfile/minified/binary paths before
	// sizing and review (REVIEW_EXCLUDE_GLOBS). Defaults to defaultReviewExcludeGlobs.
	ReviewExcludeGlobs []string
	// ReviewMaxFiles / ReviewMaxDiffBytes are the two-dimensional size-gate caps
	// (REVIEW_MAX_FILES, REVIEW_MAX_DIFF_BYTES): a PR over either cap (measured on the
	// filtered diff) is denied, not degraded. A non-positive value disables that dimension.
	// Defaults are pilot-tunable (Decision 4 — derived from the code model's context budget,
	// not a fixed number).
	ReviewMaxFiles     int
	ReviewMaxDiffBytes int

	// Execution transport (webhook → dispatcher). TasksBackend selects in-process (default)
	// or Cloud Tasks. The Cloud Tasks settings locate the queue and the worker endpoint; the
	// task carries InternalToken as its Bearer credential (no new auth var). See
	// specs/20260626-workflow-execution-transport.md.
	TasksBackend TasksBackend
	// TasksProject is the GCP project owning the queue (TASKS_PROJECT); empty falls back to
	// GOOGLE_CLOUD_PROJECT. Required for cloudtasks.
	TasksProject string
	// TasksLocation is the queue's region (TASKS_LOCATION, e.g. "us-central1"). Required for
	// cloudtasks.
	TasksLocation string
	// TasksQueue is the Cloud Tasks queue name (TASKS_QUEUE). Required for cloudtasks.
	TasksQueue string
	// DispatchURL is the full URL of the /internal/dispatch worker the queue POSTs to
	// (DISPATCH_URL, e.g. https://agent-xyz.run.app/internal/dispatch). Required for cloudtasks.
	DispatchURL string
	// TasksDispatchDeadline is how long Cloud Tasks waits for an /internal/dispatch attempt
	// before cancelling it and retrying (TASKS_DISPATCH_DEADLINE). It must be set explicitly
	// on the task: the HTTP-target default is only 10m, so a longer workflow would be
	// cancelled mid-run and retried, duplicating side effects. Cloud Tasks caps this at 30m,
	// which is therefore the default and the ceiling. Used only in cloudtasks mode.
	TasksDispatchDeadline time.Duration
}

// GitHubApp holds the GitHub App installation-token credentials, resolved at load
// time. It is populated only in App mode (production); in PAT mode it is the zero
// value. PrivateKeyPEM holds the literal PEM bytes (sourced from the literal env
// var or the key file), already unescaped and validated to parse.
type GitHubApp struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}

// AppMode reports whether GitHub App authentication is configured (production
// path). False means the static PAT fallback is used.
func (c Config) AppMode() bool { return c.GitHubApp.AppID != 0 }

// String renders the config with every credential masked, so a debug/startup log
// of it never leaks a secret. The default %+v would otherwise print the PAT,
// webhook secret, internal token, webhook URLs, and (via GitHubApp) the App
// private key verbatim. The plain alias drops this String method to avoid
// infinite recursion. Keep the masked set in sync when adding a secret field.
// (Mirrors Python's __repr__, JS's inspect redaction, and Kotlin's toString.)
func (c Config) String() string {
	type plain Config
	p := plain(c)
	p.GitHubToken = redactSecret(c.GitHubToken)
	p.GitHubWebhookSecret = redactSecret(c.GitHubWebhookSecret)
	p.InternalToken = redactSecret(c.InternalToken)
	p.SlackWebhookURL = redactSecret(c.SlackWebhookURL)
	p.TeamsWebhookURL = redactSecret(c.TeamsWebhookURL)
	// The nested GitHubApp prints through its own String, which masks the key.
	return fmt.Sprintf("%+v", p)
}

// String masks the App private key so it never reaches a log when a GitHubApp is
// printed (nested in Config or on its own); the numeric ids are not secret. The key
// is hand-formatted rather than %+v'd because a []byte renders as raw byte numbers
// — recoverable, and never the readable mask.
func (a GitHubApp) String() string {
	key := ""
	if len(a.PrivateKeyPEM) > 0 {
		key = "***"
	}
	return fmt.Sprintf("{AppID:%d InstallationID:%d PrivateKeyPEM:%s}", a.AppID, a.InstallationID, key)
}

// redactSecret masks a secret for String: an unset value stays visibly empty, a
// set value collapses to a fixed marker so its bytes never reach a log.
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "***"
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
	// Skipped in App mode: the App provider mints its own tokens, so shelling out to
	// gh would be a useless subprocess that could also hydrate a PAT the deployment
	// never asked for.
	if !c.AppMode() && c.GitHubToken == "" {
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
		ReviewExcludeGlobs:  splitList(getOr(get, "REVIEW_EXCLUDE_GLOBS", defaultReviewExcludeGlobs)),
		TasksBackend:        TasksBackend(getOr(get, "TASKS_BACKEND", string(TasksInProcess))),
		TasksProject:        getOr(get, "TASKS_PROJECT", getOr(get, "GOOGLE_CLOUD_PROJECT", "")),
		TasksLocation:       getOr(get, "TASKS_LOCATION", ""),
		TasksQueue:          getOr(get, "TASKS_QUEUE", ""),
		DispatchURL:         getOr(get, "DISPATCH_URL", ""),
	}

	var err error
	if c.ReviewEnabled, err = getBool(get, "REVIEW_ENABLED", false); err != nil {
		return Config{}, err
	}
	if c.ReviewSkipDrafts, err = getBool(get, "REVIEW_SKIP_DRAFTS", true); err != nil {
		return Config{}, err
	}
	if c.ReviewMaxFiles, err = getInt(get, "REVIEW_MAX_FILES", defaultReviewMaxFiles); err != nil {
		return Config{}, err
	}
	if c.ReviewMaxDiffBytes, err = getInt(get, "REVIEW_MAX_DIFF_BYTES", defaultReviewMaxDiffBytes); err != nil {
		return Config{}, err
	}
	if c.MaxIterations, err = strconv.Atoi(getOr(get, "MAX_ITERATIONS", "3")); err != nil {
		return Config{}, fmt.Errorf("MAX_ITERATIONS: %w", err)
	}
	if c.CITimeout, err = time.ParseDuration(getOr(get, "CI_TIMEOUT", "90m")); err != nil {
		return Config{}, fmt.Errorf("CI_TIMEOUT: %w", err)
	}
	if c.TasksDispatchDeadline, err = time.ParseDuration(getOr(get, "TASKS_DISPATCH_DEADLINE", "30m")); err != nil {
		return Config{}, fmt.Errorf("TASKS_DISPATCH_DEADLINE: %w", err)
	}

	// Code models default to the base models when unset.
	if c.OllamaCodeModel == "" {
		c.OllamaCodeModel = c.OllamaModel
	}
	if c.GeminiCodeModel == "" {
		c.GeminiCodeModel = c.GeminiModel
	}

	// Resolve GitHub App credentials (production auth path). Absent App vars leave
	// GitHubApp zero — PAT mode. Partial/misconfigured App vars are a startup error,
	// never a silent fallback (Decision §4).
	if c.GitHubApp, err = resolveGitHubApp(get); err != nil {
		return Config{}, err
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
	switch c.TasksBackend {
	case TasksInProcess:
	case TasksCloudTasks:
		// Cloud Tasks needs the queue coordinates and worker URL, plus the Bearer token the
		// task carries: without INTERNAL_TOKEN, /internal/dispatch is disabled (404) and every
		// task would fail permanently. Fail fast rather than silently never dispatching.
		var missing []string
		if c.TasksProject == "" {
			missing = append(missing, "TASKS_PROJECT (or GOOGLE_CLOUD_PROJECT)")
		}
		if c.TasksLocation == "" {
			missing = append(missing, "TASKS_LOCATION")
		}
		if c.TasksQueue == "" {
			missing = append(missing, "TASKS_QUEUE")
		}
		// DISPATCH_URL must be an absolute https URL: the Cloud Tasks task carries
		// INTERNAL_TOKEN as a Bearer header to it, so a plaintext http:// target would leak
		// the token in transit (same posture as gitrepo refusing a token over http://). It must
		// also resolve to the /internal/dispatch worker route — a base URL or a stray path
		// would pass the scheme check and then 404 every task at runtime. A suffix match (not
		// equality) tolerates a gateway path prefix while still requiring the dispatch path.
		switch u, err := url.Parse(c.DispatchURL); {
		case c.DispatchURL == "":
			missing = append(missing, "DISPATCH_URL")
		case err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" || !strings.HasSuffix(u.Path, "/internal/dispatch"):
			missing = append(missing, "DISPATCH_URL (must be an absolute https:// URL ending in /internal/dispatch)")
		}
		if c.InternalToken == "" {
			missing = append(missing, "INTERNAL_TOKEN (the Bearer the task carries to /internal/dispatch)")
		}
		// Cloud Tasks clamps an HTTP-target dispatch deadline to 15s..30m; a value outside that
		// range is silently rejected at CreateTask, so reject it here instead.
		if c.TasksDispatchDeadline < 15*time.Second || c.TasksDispatchDeadline > 30*time.Minute {
			missing = append(missing, "TASKS_DISPATCH_DEADLINE (must be between 15s and 30m)")
		}
		// In Cloud Tasks mode the deployment is production-facing, so an unverified webhook
		// surface is a real exposure rather than a dev convenience — require the HMAC secret
		// (it stays an opt-in warning only for the local inprocess default).
		if c.GitHubWebhookSecret == "" {
			missing = append(missing, "GITHUB_WEBHOOK_SECRET (webhook signatures must be verified in production)")
		}
		if len(missing) > 0 {
			return fmt.Errorf("TASKS_BACKEND=cloudtasks requires %s", strings.Join(missing, ", "))
		}
	default:
		return fmt.Errorf("invalid TASKS_BACKEND %q (want inprocess|cloudtasks)", c.TasksBackend)
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
	// In App mode an installation can see every repo it is installed on, so an empty
	// allow-list ("act on all repos", the PAT-mode default) is a footgun — fail fast
	// (Decision §3). PAT mode keeps "empty = all" for local-dev back-compat.
	if c.AppMode() && len(c.Repos) == 0 {
		return errors.New("REPOS must be set in GitHub App mode (empty REPOS = all repos is rejected to avoid acting on every installed repo)")
	}
	return nil
}

// resolveGitHubApp reads the GITHUB_APP_* vars and decides the auth mode. With none
// set, it returns the zero value (PAT mode). With any set, App mode is intended and
// every requirement is enforced — App ID, a pinned installation id, and exactly one
// private-key source — so a partial configuration is a startup error, not a silent
// fallback to PAT (mode-selection rule, spec §"Config / env" + Decision §4).
func resolveGitHubApp(get lookup) (GitHubApp, error) {
	appIDStr := getOr(get, "GITHUB_APP_ID", "")
	installIDStr := getOr(get, "GITHUB_APP_INSTALLATION_ID", "")
	keyLiteral := getOr(get, "GITHUB_APP_PRIVATE_KEY", "")
	keyPath := getOr(get, "GITHUB_APP_PRIVATE_KEY_PATH", "")

	if appIDStr == "" && installIDStr == "" && keyLiteral == "" && keyPath == "" {
		return GitHubApp{}, nil // PAT mode — no App vars present.
	}
	// Any App var present signals intent to use App mode; require the full set.
	if appIDStr == "" {
		return GitHubApp{}, errors.New("GITHUB_APP_* set but GITHUB_APP_ID is missing (App mode requires GITHUB_APP_ID)")
	}
	if installIDStr == "" {
		return GitHubApp{}, errors.New("App mode requires GITHUB_APP_INSTALLATION_ID (single pinned installation)")
	}
	switch {
	case keyLiteral != "" && keyPath != "":
		return GitHubApp{}, errors.New("set exactly one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH, not both")
	case keyLiteral == "" && keyPath == "":
		return GitHubApp{}, errors.New("App mode requires one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH")
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return GitHubApp{}, fmt.Errorf("GITHUB_APP_ID must be numeric, got %q", appIDStr)
	}
	// A non-positive App ID is invalid and, worse, 0 would collide with AppMode's
	// zero-value sentinel and silently fall back to PAT — reject it explicitly.
	if appID <= 0 {
		return GitHubApp{}, fmt.Errorf("GITHUB_APP_ID must be > 0, got %d", appID)
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		return GitHubApp{}, fmt.Errorf("GITHUB_APP_INSTALLATION_ID must be numeric, got %q", installIDStr)
	}
	if installID <= 0 {
		return GitHubApp{}, fmt.Errorf("GITHUB_APP_INSTALLATION_ID must be > 0, got %d", installID)
	}

	raw := []byte(keyLiteral)
	if keyPath != "" {
		if raw, err = os.ReadFile(keyPath); err != nil {
			return GitHubApp{}, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_PATH %q: %w", keyPath, err)
		}
	}
	pemBytes, err := normalizePrivateKeyPEM(raw)
	if err != nil {
		return GitHubApp{}, err
	}
	return GitHubApp{AppID: appID, InstallationID: installID, PrivateKeyPEM: pemBytes}, nil
}

// normalizePrivateKeyPEM makes the App private key robust to how it is delivered
// (Decision §4): CI secret stores often flatten newlines to the literal characters
// `\n`, so when the value looks like PEM and contains escaped `\n` sequences, restore
// them — even if a real trailing newline is also present.
// It then validates the key parses as an RSA private key, failing at startup with a
// clear message rather than a cryptic RS256 error at first token exchange.
func normalizePrivateKeyPEM(raw []byte) ([]byte, error) {
	if bytes.Contains(raw, []byte("-----BEGIN")) && bytes.Contains(raw, []byte(`\n`)) {
		raw = bytes.ReplaceAll(raw, []byte(`\n`), []byte("\n"))
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("GitHub App private key is not valid PEM (no PEM block found)")
	}
	// GitHub App keys are RSA, and RS256 JWT signing requires an RSA key. Accept a
	// PKCS#1 key, or a PKCS#8 key only if it is specifically RSA — reject EC/Ed25519
	// here rather than failing cryptically at the first token exchange.
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		key, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("GitHub App private key does not parse as an RSA key: %w", err)
		}
		if _, ok := key.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("GitHub App private key must be RSA, got %T", key)
		}
	}
	return raw, nil
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

// getBool parses a boolean env var (REVIEW_ENABLED etc.). Unset or blank yields def; a
// set-but-unparseable value is a startup error rather than a silent default, matching the
// strict handling of the numeric/duration vars.
func getBool(get lookup, key string, def bool) (bool, error) {
	v := getOr(get, key, "")
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// getInt parses an integer env var (REVIEW_MAX_FILES etc.). Unset or blank yields def; a
// set-but-unparseable value is a startup error, matching getBool's strictness.
func getInt(get lookup, key string, def int) (int, error) {
	v := getOr(get, key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// Reviewer intake defaults (pilot-tunable).
const (
	// defaultReviewMaxFiles / defaultReviewMaxDiffBytes are the size-gate caps a PR must stay
	// under to be reviewed (measured on the filtered diff). Beyond either, the PR is denied
	// rather than degraded (spec Decision 4).
	defaultReviewMaxFiles     = 50
	defaultReviewMaxDiffBytes = 256 * 1024 // 256 KiB

	// defaultReviewExcludeGlobs are the paths dropped before sizing/review: lockfiles,
	// generated code, vendored trees, minified bundles, snapshots, and binaries. A pattern
	// with no '/' matches the basename; one with '/' matches the full path ("**" crosses
	// separators).
	defaultReviewExcludeGlobs = "go.sum,go.work.sum,package-lock.json,yarn.lock,pnpm-lock.yaml," +
		"npm-shrinkwrap.json,Cargo.lock,poetry.lock,Pipfile.lock,Gemfile.lock,composer.lock," +
		"gradle.lockfile,*.min.js,*.min.css,*.map,*.snap,*.pb.go,*_pb2.py,*.gen.go,*_generated.go," +
		"vendor/**,node_modules/**,third_party/**,dist/**,build/**,__snapshots__/**," +
		"*.png,*.jpg,*.jpeg,*.gif,*.webp,*.ico,*.pdf,*.woff,*.woff2,*.ttf,*.eot," +
		"*.zip,*.gz,*.tar,*.jar,*.bin,*.so,*.dylib,*.dll,*.exe"
)

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
