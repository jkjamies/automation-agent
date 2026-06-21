// Package setup holds shared utilities for building agents: the LLM provider
// switch and adapters, the prompt loader, and genai helpers. It is the only
// package permitted to import provider SDKs (Ollama, Gemini) — enforced by ARCH.
package setup

import (
	"context"
	"fmt"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/config"
)

// BuildLLM returns the default model.LLM (triage, explore, summary) for the
// configured provider. Agents depend only on the returned interface, so switching
// providers is a config change, not a code change. See docs/architecture.md §4.
func BuildLLM(ctx context.Context, cfg config.Config) (model.LLM, error) {
	return buildLLM(ctx, cfg, cfg.OllamaModel, cfg.GeminiModel)
}

// BuildCodeLLM returns the model.LLM for the code-change steps (lint rewrite,
// coverage test generation) — typically a larger model. Falls back to the default
// model when no code model is configured.
func BuildCodeLLM(ctx context.Context, cfg config.Config) (model.LLM, error) {
	return buildLLM(ctx, cfg, cfg.OllamaCodeModel, cfg.GeminiCodeModel)
}

func buildLLM(ctx context.Context, cfg config.Config, ollamaModel, geminiModel string) (model.LLM, error) {
	switch cfg.LLMProvider {
	case config.ProviderOllama:
		return NewOllamaModel(cfg.OllamaHost, ollamaModel)
	case config.ProviderGemini:
		return newGeminiModel(ctx, geminiModel)
	default:
		return nil, fmt.Errorf("unknown LLM provider %q", cfg.LLMProvider)
	}
}
