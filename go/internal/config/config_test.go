package config

import (
	"testing"
	"time"
)

func mapLookup(m map[string]string) lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := loadFrom(mapLookup(nil))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.LLMProvider != ProviderOllama {
		t.Errorf("LLMProvider = %q, want ollama", c.LLMProvider)
	}
	if c.OllamaModel != "gemma4:12b" {
		t.Errorf("OllamaModel = %q, want gemma4:12b", c.OllamaModel)
	}
	if c.OllamaCodeModel != "gemma4:26b" {
		t.Errorf("OllamaCodeModel = %q, want default gemma4:26b", c.OllamaCodeModel)
	}
	if c.NotifyProvider != NotifySlack {
		t.Errorf("NotifyProvider = %q, want slack", c.NotifyProvider)
	}
	if c.MaxIterations != 3 {
		t.Errorf("MaxIterations = %d, want 3", c.MaxIterations)
	}
	if c.CITimeout.Minutes() != 90 {
		t.Errorf("CITimeout = %v, want 90m", c.CITimeout)
	}
	if c.SessionBackend != SessionMemory {
		t.Errorf("SessionBackend = %q, want memory", c.SessionBackend)
	}
	if c.GitTransport != "https" {
		t.Errorf("GitTransport = %q, want https", c.GitTransport)
	}
	if c.TasksBackend != TasksInProcess {
		t.Errorf("TasksBackend = %q, want inprocess", c.TasksBackend)
	}
}

func TestInvalidTasksBackend(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"TASKS_BACKEND": "kafka"})); err == nil {
		t.Fatal("expected error for invalid TASKS_BACKEND")
	}
}

func TestReviewEnabled(t *testing.T) {
	c, err := loadFrom(mapLookup(nil))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.ReviewEnabled {
		t.Error("ReviewEnabled should default to false")
	}

	c, err = loadFrom(mapLookup(map[string]string{"REVIEW_ENABLED": "true"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !c.ReviewEnabled {
		t.Error("REVIEW_ENABLED=true should enable the reviewer")
	}

	if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_ENABLED": "maybe"})); err == nil {
		t.Error("an unparseable REVIEW_ENABLED should be a startup error")
	}
}

func TestReviewIntakeDefaults(t *testing.T) {
	c, err := loadFrom(mapLookup(nil))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !c.ReviewSkipDrafts {
		t.Error("ReviewSkipDrafts should default to true")
	}
	if c.ReviewMaxFiles != defaultReviewMaxFiles {
		t.Errorf("ReviewMaxFiles = %d, want default %d", c.ReviewMaxFiles, defaultReviewMaxFiles)
	}
	if c.ReviewMaxDiffBytes != defaultReviewMaxDiffBytes {
		t.Errorf("ReviewMaxDiffBytes = %d, want default %d", c.ReviewMaxDiffBytes, defaultReviewMaxDiffBytes)
	}
	if c.ReviewMinConfidence != defaultReviewMinConfidence {
		t.Errorf("ReviewMinConfidence = %v, want default %v", c.ReviewMinConfidence, defaultReviewMinConfidence)
	}
	if len(c.ReviewExcludeGlobs) == 0 {
		t.Fatal("ReviewExcludeGlobs should default to a non-empty set")
	}
	var hasLock bool
	for _, g := range c.ReviewExcludeGlobs {
		if g == "go.sum" {
			hasLock = true
		}
	}
	if !hasLock {
		t.Error("default exclude globs should include lockfiles like go.sum")
	}
}

func TestReviewIntakeOverrides(t *testing.T) {
	c, err := loadFrom(mapLookup(map[string]string{
		"REVIEW_SKIP_DRAFTS":    "false",
		"REVIEW_MAX_FILES":      "10",
		"REVIEW_MAX_DIFF_BYTES": "2048",
		"REVIEW_MIN_CONFIDENCE": "0.8",
		"REVIEW_EXCLUDE_GLOBS":  "*.foo, *.bar",
	}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.ReviewSkipDrafts {
		t.Error("REVIEW_SKIP_DRAFTS=false should disable draft skipping")
	}
	if c.ReviewMaxFiles != 10 || c.ReviewMaxDiffBytes != 2048 {
		t.Errorf("caps = %d / %d, want 10 / 2048", c.ReviewMaxFiles, c.ReviewMaxDiffBytes)
	}
	if c.ReviewMinConfidence != 0.8 {
		t.Errorf("ReviewMinConfidence = %v, want 0.8", c.ReviewMinConfidence)
	}
	if len(c.ReviewExcludeGlobs) != 2 || c.ReviewExcludeGlobs[0] != "*.foo" || c.ReviewExcludeGlobs[1] != "*.bar" {
		t.Errorf("ReviewExcludeGlobs = %v, want [*.foo *.bar]", c.ReviewExcludeGlobs)
	}
}

func TestReviewMaxFilesUnparseable(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_MAX_FILES": "lots"})); err == nil {
		t.Error("an unparseable REVIEW_MAX_FILES should be a startup error")
	}
}

func TestReviewMinConfidenceUnparseable(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_MIN_CONFIDENCE": "high"})); err == nil {
		t.Error("an unparseable REVIEW_MIN_CONFIDENCE should be a startup error")
	}
}

// REVIEW_MIN_CONFIDENCE must be a probability in [0,1]; non-finite or out-of-range values are
// rejected at startup rather than silently dropping every finding (>1) or filtering on NaN.
func TestReviewMinConfidenceOutOfRange(t *testing.T) {
	for _, v := range []string{"1.5", "-0.1", "NaN", "+Inf", "Inf"} {
		if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_MIN_CONFIDENCE": v})); err == nil {
			t.Errorf("REVIEW_MIN_CONFIDENCE=%q should be a startup error", v)
		}
	}
	// Boundaries are valid.
	for _, v := range []string{"0", "1", "0.6"} {
		if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_MIN_CONFIDENCE": v})); err != nil {
			t.Errorf("REVIEW_MIN_CONFIDENCE=%q should be valid, got %v", v, err)
		}
	}
}

// A complete, valid cloudtasks configuration (used as the base for negative cases).
func fullCloudTasksEnv() map[string]string {
	return map[string]string{
		"TASKS_BACKEND": "cloudtasks", "TASKS_PROJECT": "proj", "TASKS_LOCATION": "us-central1",
		"TASKS_QUEUE": "agent-q", "DISPATCH_URL": "https://svc.run.app/internal/dispatch",
		"INTERNAL_TOKEN": "sekret", "GITHUB_WEBHOOK_SECRET": "hmac",
	}
}

// cloudtasks mode requires the queue coordinates, the worker URL, the Bearer token, and a
// verified webhook surface — each omission is a startup error.
func TestCloudTasksRequiresSettings(t *testing.T) {
	// Missing everything but the backend selector.
	if _, err := loadFrom(mapLookup(map[string]string{"TASKS_BACKEND": "cloudtasks"})); err == nil {
		t.Fatal("expected error: cloudtasks with no settings")
	}
	// Drop one required key at a time from an otherwise-valid config.
	for _, key := range []string{"TASKS_LOCATION", "TASKS_QUEUE", "DISPATCH_URL", "INTERNAL_TOKEN", "GITHUB_WEBHOOK_SECRET"} {
		env := fullCloudTasksEnv()
		delete(env, key)
		if _, err := loadFrom(mapLookup(env)); err == nil {
			t.Errorf("expected error: cloudtasks without %s", key)
		}
	}
}

// DISPATCH_URL must be an absolute https URL that targets the /internal/dispatch worker. A
// plaintext http:// target (which would leak the Bearer token) and a base URL or wrong path
// (which would 404 every task at runtime) are both rejected.
func TestCloudTasksRejectsInsecureDispatchURL(t *testing.T) {
	for _, bad := range []string{
		"http://svc.run.app/internal/dispatch", // plaintext leaks the token
		"/internal/dispatch",                   // not absolute
		"not a url",                            // unparseable
		"https://svc.run.app",                  // base URL, missing the worker path
		"https://svc.run.app/internal/sweep",   // wrong path
	} {
		env := fullCloudTasksEnv()
		env["DISPATCH_URL"] = bad
		if _, err := loadFrom(mapLookup(env)); err == nil {
			t.Errorf("expected error for DISPATCH_URL=%q", bad)
		}
	}
	// A gateway path prefix in front of /internal/dispatch is tolerated (suffix match).
	env := fullCloudTasksEnv()
	env["DISPATCH_URL"] = "https://gw.example.com/agent/internal/dispatch"
	if _, err := loadFrom(mapLookup(env)); err != nil {
		t.Errorf("gateway-prefixed dispatch URL should be accepted: %v", err)
	}
}

// TASKS_DISPATCH_DEADLINE must fall within Cloud Tasks' 15s..30m HTTP-target range; outside
// it (or unparseable) is rejected, and the default is the 30m maximum.
func TestCloudTasksDispatchDeadline(t *testing.T) {
	for _, bad := range []string{"5s", "31m", "garbage"} {
		env := fullCloudTasksEnv()
		env["TASKS_DISPATCH_DEADLINE"] = bad
		if _, err := loadFrom(mapLookup(env)); err == nil {
			t.Errorf("expected error for TASKS_DISPATCH_DEADLINE=%q", bad)
		}
	}
	c, err := loadFrom(mapLookup(fullCloudTasksEnv()))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.TasksDispatchDeadline != 30*time.Minute {
		t.Errorf("default TasksDispatchDeadline = %v, want 30m", c.TasksDispatchDeadline)
	}
}

func TestCloudTasksFullConfig(t *testing.T) {
	c, err := loadFrom(mapLookup(fullCloudTasksEnv()))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.TasksBackend != TasksCloudTasks {
		t.Errorf("TasksBackend = %q, want cloudtasks", c.TasksBackend)
	}
	if c.TasksProject != "proj" || c.TasksLocation != "us-central1" || c.TasksQueue != "agent-q" {
		t.Errorf("queue coords = %q/%q/%q", c.TasksProject, c.TasksLocation, c.TasksQueue)
	}
	if c.DispatchURL != "https://svc.run.app/internal/dispatch" {
		t.Errorf("DispatchURL = %q", c.DispatchURL)
	}
}

// TASKS_PROJECT falls back to GOOGLE_CLOUD_PROJECT (the ambient Cloud Run var).
func TestTasksProjectFallsBackToGoogleCloudProject(t *testing.T) {
	env := fullCloudTasksEnv()
	delete(env, "TASKS_PROJECT")
	env["GOOGLE_CLOUD_PROJECT"] = "ambient"
	c, err := loadFrom(mapLookup(env))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.TasksProject != "ambient" {
		t.Errorf("TasksProject = %q, want ambient (from GOOGLE_CLOUD_PROJECT)", c.TasksProject)
	}
}

func TestGitTransportSSH(t *testing.T) {
	c, err := loadFrom(mapLookup(map[string]string{"GIT_TRANSPORT": "ssh", "GIT_SSH_KEY": "/home/dev/.ssh/id_ed25519"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.GitTransport != "ssh" {
		t.Errorf("GitTransport = %q, want ssh", c.GitTransport)
	}
	if c.GitSSHKey != "/home/dev/.ssh/id_ed25519" {
		t.Errorf("GitSSHKey = %q, want the configured path", c.GitSSHKey)
	}
}

func TestInvalidGitTransport(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"GIT_TRANSPORT": "scp"})); err == nil {
		t.Fatal("expected error for invalid GIT_TRANSPORT")
	}
}

func TestInvalidSessionBackend(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"SESSION_BACKEND": "redis"})); err == nil {
		t.Fatal("expected error for invalid SESSION_BACKEND")
	}
}

func TestSessionBackendOverride(t *testing.T) {
	c, err := loadFrom(mapLookup(map[string]string{"SESSION_BACKEND": "sqlite"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.SessionBackend != SessionSQLite {
		t.Errorf("SessionBackend = %q, want sqlite", c.SessionBackend)
	}
}

func TestReposParsing(t *testing.T) {
	c, err := loadFrom(mapLookup(map[string]string{"REPOS": " a/b , c/d ,, e/f "}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	want := []string{"a/b", "c/d", "e/f"}
	if len(c.Repos) != len(want) {
		t.Fatalf("Repos = %v, want %v", c.Repos, want)
	}
	for i := range want {
		if c.Repos[i] != want[i] {
			t.Errorf("Repos[%d] = %q, want %q", i, c.Repos[i], want[i])
		}
	}
}

func TestCodeModelOverride(t *testing.T) {
	c, err := loadFrom(mapLookup(map[string]string{"OLLAMA_MODEL": "gemma4:12b", "OLLAMA_CODE_MODEL": "gemma4:26b"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.OllamaCodeModel != "gemma4:26b" {
		t.Errorf("OllamaCodeModel = %q, want gemma4:26b", c.OllamaCodeModel)
	}
	if c.OllamaModel != "gemma4:12b" {
		t.Errorf("OllamaModel = %q, want gemma4:12b", c.OllamaModel)
	}
}

func TestGitHubTokenEnvChain(t *testing.T) {
	// GH_TOKEN is honoured when GITHUB_TOKEN is unset, so a local gh-style env works.
	c, err := loadFrom(mapLookup(map[string]string{"GH_TOKEN": "gh_abc"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.GitHubToken != "gh_abc" {
		t.Errorf("GitHubToken = %q, want gh_abc", c.GitHubToken)
	}

	// GITHUB_TOKEN takes precedence over GH_TOKEN.
	c, err = loadFrom(mapLookup(map[string]string{"GITHUB_TOKEN": "primary", "GH_TOKEN": "fallback"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.GitHubToken != "primary" {
		t.Errorf("GitHubToken = %q, want primary", c.GitHubToken)
	}
}

func TestInvalidProvider(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"LLM_PROVIDER": "openai"})); err == nil {
		t.Fatal("expected error for invalid LLM_PROVIDER")
	}
}

func TestInvalidNotify(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"NOTIFY_PROVIDER": "discord"})); err == nil {
		t.Fatal("expected error for invalid NOTIFY_PROVIDER")
	}
}

func TestInvalidDuration(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"CI_TIMEOUT": "soon"})); err == nil {
		t.Fatal("expected error for invalid CI_TIMEOUT")
	}
}

func TestCITimeoutMustBePositive(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"CI_TIMEOUT": "0s"})); err == nil {
		t.Fatal("expected error for CI_TIMEOUT=0s")
	}
	if _, err := loadFrom(mapLookup(map[string]string{"CI_TIMEOUT": "-5m"})); err == nil {
		t.Fatal("expected error for negative CI_TIMEOUT")
	}
}

func TestMaxIterationsFloor(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"MAX_ITERATIONS": "0"})); err == nil {
		t.Fatal("expected error for MAX_ITERATIONS=0")
	}
}

func TestInvalidPort(t *testing.T) {
	if _, err := loadFrom(mapLookup(map[string]string{"PORT": "abc"})); err == nil {
		t.Fatal("expected error for non-numeric PORT")
	}
	if _, err := loadFrom(mapLookup(map[string]string{"PORT": "70000"})); err == nil {
		t.Fatal("expected error for out-of-range PORT")
	}
}

func TestReviewDebounce(t *testing.T) {
	c, err := loadFrom(mapLookup(nil))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.ReviewDebounce != 30*time.Second {
		t.Errorf("default ReviewDebounce = %v, want 30s", c.ReviewDebounce)
	}
	c, err = loadFrom(mapLookup(map[string]string{"REVIEW_DEBOUNCE": "5s"}))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if c.ReviewDebounce != 5*time.Second {
		t.Errorf("ReviewDebounce = %v, want 5s", c.ReviewDebounce)
	}
	if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_DEBOUNCE": "soon"})); err == nil {
		t.Error("an unparseable REVIEW_DEBOUNCE should be a startup error")
	}
}

func TestReviewStandardsConfig(t *testing.T) {
	c, err := loadFrom(mapLookup(nil))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !c.ReviewStandards {
		t.Error("REVIEW_STANDARDS should default true")
	}
	if c.ReviewUncitedMode != "nitpick" {
		t.Errorf("REVIEW_UNCITED_MODE default = %q, want nitpick", c.ReviewUncitedMode)
	}
	if len(c.ReviewStandardsGlobs) == 0 {
		t.Error("REVIEW_STANDARDS_GLOBS should have defaults")
	}
	if c.ReviewStandardsMaxBytes <= 0 {
		t.Error("REVIEW_STANDARDS_MAX_BYTES should default positive")
	}
	// An invalid mode is a startup error, not a silent default.
	if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_UNCITED_MODE": "bogus"})); err == nil {
		t.Error("invalid REVIEW_UNCITED_MODE must be a startup error")
	}
	// A non-positive byte cap fetches no docs, so it is rejected at startup.
	for _, v := range []string{"0", "-1"} {
		if _, err := loadFrom(mapLookup(map[string]string{"REVIEW_STANDARDS_MAX_BYTES": v})); err == nil {
			t.Errorf("REVIEW_STANDARDS_MAX_BYTES=%q must be a startup error", v)
		}
	}
}
