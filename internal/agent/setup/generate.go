package setup

import (
	"context"
	"strings"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// GenerateText runs a single non-streaming completion: system is the instruction,
// user is the prompt, and the concatenated text response is returned. It lets
// callers outside this package use an LLM without importing genai directly.
func GenerateText(ctx context.Context, llm model.LLM, system, user string) (string, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{UserText(user)},
		Config:   &genai.GenerateContentConfig{SystemInstruction: UserText(system)},
	}
	var sb strings.Builder
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", err
		}
		if resp.Content != nil {
			sb.WriteString(contentText(resp.Content))
		}
	}
	return sb.String(), nil
}
