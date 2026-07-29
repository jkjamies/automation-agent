package setup

import (
	"context"
	"fmt"
	"net/http"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"automation-agent/internal/useragent"
)

// newGeminiModel builds the Gemini-backed model.LLM for the cloud deployment.
// Credentials/backend are read from the environment by the genai client (API key
// or Vertex via GOOGLE_GENAI_USE_VERTEXAI / GOOGLE_CLOUD_PROJECT).
func newGeminiModel(ctx context.Context, geminiModel string) (model.LLM, error) {
	if geminiModel == "" {
		return nil, fmt.Errorf("GEMINI_MODEL must be set when LLM_PROVIDER=gemini")
	}
	// Supply the HTTP client purely to brand the traffic: leaving it nil lets the SDK build its
	// own, which authenticates identically but goes out unidentified. Only the transport is set,
	// so the SDK's own timeout and retry behaviour are untouched.
	return gemini.NewModel(ctx, geminiModel, &genai.ClientConfig{
		HTTPClient: &http.Client{Transport: useragent.Transport(nil)},
	})
}
