package setup

import (
	"context"
	"testing"

	"automation-agent/internal/config"
)

func TestBuildLLMOllama(t *testing.T) {
	cfg := config.Config{
		LLMProvider: config.ProviderOllama,
		OllamaHost:  "http://localhost:11434",
		OllamaModel: "gemma4:12b",
	}
	m, err := BuildLLM(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildLLM: %v", err)
	}
	if m.Name() != "gemma4:12b" {
		t.Errorf("Name = %q, want gemma4:12b", m.Name())
	}
}

func TestBuildLLMGeminiRequiresModel(t *testing.T) {
	cfg := config.Config{LLMProvider: config.ProviderGemini}
	if _, err := BuildLLM(context.Background(), cfg); err == nil {
		t.Fatal("expected error when GEMINI_MODEL is empty")
	}
}

func TestBuildLLMUnknownProvider(t *testing.T) {
	cfg := config.Config{LLMProvider: config.Provider("openai")}
	if _, err := BuildLLM(context.Background(), cfg); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
