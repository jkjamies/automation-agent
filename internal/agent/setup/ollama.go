package setup

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"github.com/ollama/ollama/api"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// OllamaModel adapts a local Ollama server to the adk model.LLM interface so
// agents can run against Gemma locally. It is the keystone of local-first dev:
// the rest of the system depends only on model.LLM, never on Ollama directly.
type OllamaModel struct {
	client *api.Client
	name   string
}

var _ model.LLM = (*OllamaModel)(nil)

// NewOllamaModel builds an adapter pointing at host (e.g. http://localhost:11434)
// for the given model tag (e.g. gemma4:12b).
func NewOllamaModel(host, modelTag string) (*OllamaModel, error) {
	if modelTag == "" {
		return nil, errors.New("ollama model tag must not be empty")
	}
	base, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse ollama host %q: %w", host, err)
	}
	return &OllamaModel{client: api.NewClient(base, http.DefaultClient), name: modelTag}, nil
}

// Name reports the configured model tag.
func (m *OllamaModel) Name() string { return m.name }

// errStopIteration unwinds the Ollama streaming callback when the consumer of the
// iterator stops early; it is never surfaced to the caller.
var errStopIteration = errors.New("setup: iteration stopped by consumer")

// GenerateContent implements model.LLM. In streaming mode it yields each chunk as
// a Partial response and a final aggregated response with TurnComplete set; in
// non-streaming mode it yields a single complete response.
func (m *OllamaModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		streamFlag := stream
		chatReq := &api.ChatRequest{
			Model:    m.modelName(req),
			Messages: toOllamaMessages(req),
			Stream:   &streamFlag,
		}

		var full strings.Builder
		err := m.client.Chat(ctx, chatReq, func(resp api.ChatResponse) error {
			full.WriteString(resp.Message.Content)

			if stream && !resp.Done {
				partial := newTextResponse(resp.Message.Content, resp.Model)
				partial.Partial = true
				if !yield(partial, nil) {
					return errStopIteration
				}
				return nil
			}

			if resp.Done {
				final := newTextResponse(full.String(), resp.Model)
				final.TurnComplete = true
				final.FinishReason = genai.FinishReasonStop
				if !yield(final, nil) {
					return errStopIteration
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopIteration) {
			yield(nil, fmt.Errorf("ollama chat: %w", err))
		}
	}
}

// modelName prefers req.Model (which a BeforeModelCallback may set) over the
// construction-time default.
func (m *OllamaModel) modelName(req *model.LLMRequest) string {
	if req != nil && req.Model != "" {
		return req.Model
	}
	return m.name
}

func newTextResponse(text, modelVersion string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		ModelVersion: modelVersion,
	}
}

// toOllamaMessages flattens the genai system instruction + contents into Ollama
// chat messages. The genai "model" role maps to Ollama's "assistant".
func toOllamaMessages(req *model.LLMRequest) []api.Message {
	var msgs []api.Message
	if req.Config != nil && req.Config.SystemInstruction != nil {
		if t := contentText(req.Config.SystemInstruction); t != "" {
			msgs = append(msgs, api.Message{Role: "system", Content: t})
		}
	}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		role := "user"
		if c.Role == genai.RoleModel {
			role = "assistant"
		}
		msgs = append(msgs, api.Message{Role: role, Content: contentText(c)})
	}
	return msgs
}

func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
