package setup

import (
	"context"
	"fmt"

	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/genai"

	"github.com/jkjamies/automation-agent/internal/config"
)

// newGeminiModel builds the Gemini-backed model.LLM for the cloud deployment.
// Credentials/backend are read from the environment by the genai client (API key
// or Vertex via GOOGLE_GENAI_USE_VERTEXAI / GOOGLE_CLOUD_PROJECT).
func newGeminiModel(ctx context.Context, cfg config.Config) (model.LLM, error) {
	if cfg.GeminiModel == "" {
		return nil, fmt.Errorf("GEMINI_MODEL must be set when LLM_PROVIDER=gemini")
	}
	return gemini.NewModel(ctx, cfg.GeminiModel, &genai.ClientConfig{})
}
