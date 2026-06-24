package config

import "testing"

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
