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
	if c.NotifyProvider != NotifySlack {
		t.Errorf("NotifyProvider = %q, want slack", c.NotifyProvider)
	}
	if c.MaxIterations != 3 {
		t.Errorf("MaxIterations = %d, want 3", c.MaxIterations)
	}
	if c.CITimeout.Minutes() != 90 {
		t.Errorf("CITimeout = %v, want 90m", c.CITimeout)
	}
	if c.AgentPRLabel != "automation-agent" {
		t.Errorf("AgentPRLabel = %q", c.AgentPRLabel)
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
