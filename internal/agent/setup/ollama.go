package setup

import (
	"context"
	"encoding/json"
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

// defaultNumCtx is the context window requested from Ollama. gemma4 is served with a
// 32k window; setting it (with Truncate=false) avoids the server default (~4k) that
// would silently chop large file prompts.
const defaultNumCtx = 32768

// OllamaModel adapts a local Ollama server to the adk model.LLM interface so agents
// can run against Gemma locally. It honors GenerateContentConfig (temperature,
// num_ctx, JSON format) and tool declarations, so tool-using agents work locally.
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

var errStopIteration = errors.New("setup: iteration stopped by consumer")

// GenerateContent implements model.LLM. It forwards generation options and tools,
// aggregates streaming chunks, and surfaces tool calls as genai function-call parts.
func (m *OllamaModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		streamFlag := stream
		noTruncate := false // fail loudly rather than silently truncate an oversized prompt
		chatReq := &api.ChatRequest{
			Model:    m.modelName(req),
			Messages: toOllamaMessages(req),
			Stream:   &streamFlag,
			Options:  generationOptions(req),
			Tools:    toOllamaTools(req),
			Truncate: &noTruncate,
		}
		if wantsJSON(req) {
			chatReq.Format = json.RawMessage(`"json"`)
		}

		var full strings.Builder
		var toolCalls []api.ToolCall
		err := m.client.Chat(ctx, chatReq, func(resp api.ChatResponse) error {
			full.WriteString(resp.Message.Content)
			toolCalls = append(toolCalls, resp.Message.ToolCalls...)

			if stream && !resp.Done {
				if strings.TrimSpace(resp.Message.Content) == "" {
					return nil
				}
				partial := newTextResponse(resp.Message.Content, resp.Model)
				partial.Partial = true
				if !yield(partial, nil) {
					return errStopIteration
				}
				return nil
			}
			if resp.Done {
				if !yield(finalResponse(full.String(), toolCalls, resp.Model), nil) {
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

// modelName prefers req.Model (which a BeforeModelCallback may set) over the default.
func (m *OllamaModel) modelName(req *model.LLMRequest) string {
	if req != nil && req.Model != "" {
		return req.Model
	}
	return m.name
}

// generationOptions maps GenerateContentConfig onto Ollama options. Temperature
// defaults to 0 for deterministic code/JSON; num_ctx is set so large files aren't
// truncated.
func generationOptions(req *model.LLMRequest) map[string]any {
	opts := map[string]any{"num_ctx": defaultNumCtx, "temperature": 0.0}
	if req == nil || req.Config == nil {
		return opts
	}
	if req.Config.Temperature != nil {
		opts["temperature"] = float64(*req.Config.Temperature)
	}
	if req.Config.TopP != nil {
		opts["top_p"] = float64(*req.Config.TopP)
	}
	if req.Config.Seed != nil {
		opts["seed"] = int(*req.Config.Seed)
	}
	return opts
}

func wantsJSON(req *model.LLMRequest) bool {
	return req != nil && req.Config != nil && strings.Contains(strings.ToLower(req.Config.ResponseMIMEType), "json")
}

func newTextResponse(text, modelVersion string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		ModelVersion: modelVersion,
	}
}

// finalResponse builds the terminal response, including any tool calls as genai
// function-call parts so the runner can execute the tools.
func finalResponse(text string, toolCalls []api.ToolCall, modelVersion string) *model.LLMResponse {
	parts := make([]*genai.Part, 0, len(toolCalls)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	for _, tc := range toolCalls {
		parts = append(parts, &genai.Part{FunctionCall: toGenaiFunctionCall(tc)})
	}
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
		ModelVersion: modelVersion,
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

func toGenaiFunctionCall(tc api.ToolCall) *genai.FunctionCall {
	args := map[string]any{}
	if b, err := json.Marshal(tc.Function.Arguments); err == nil {
		_ = json.Unmarshal(b, &args)
	}
	return &genai.FunctionCall{Name: tc.Function.Name, Args: args}
}

// toOllamaTools converts genai function declarations into Ollama tool definitions.
func toOllamaTools(req *model.LLMRequest) api.Tools {
	if req == nil || req.Config == nil {
		return nil
	}
	var tools api.Tools
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd == nil {
				continue
			}
			tools = append(tools, api.Tool{
				Type: "function",
				Function: api.ToolFunction{
					Name:        fd.Name,
					Description: fd.Description,
					Parameters:  toToolParams(fd.Parameters),
				},
			})
		}
	}
	return tools
}

func toToolParams(s *genai.Schema) api.ToolFunctionParameters {
	p := api.ToolFunctionParameters{Type: "object"}
	if s == nil {
		return p
	}
	if s.Type != "" {
		p.Type = strings.ToLower(string(s.Type))
	}
	p.Required = s.Required
	if len(s.Properties) > 0 {
		props := api.NewToolPropertiesMap()
		for name, ps := range s.Properties {
			props.Set(name, toToolProperty(ps))
		}
		p.Properties = props
	}
	return p
}

func toToolProperty(s *genai.Schema) api.ToolProperty {
	if s == nil {
		return api.ToolProperty{}
	}
	tp := api.ToolProperty{Description: s.Description, Required: s.Required}
	if s.Type != "" {
		tp.Type = api.PropertyType{strings.ToLower(string(s.Type))}
	}
	if s.Items != nil {
		tp.Items = toToolProperty(s.Items)
	}
	if len(s.Properties) > 0 {
		props := api.NewToolPropertiesMap()
		for name, ps := range s.Properties {
			props.Set(name, toToolProperty(ps))
		}
		tp.Properties = props
	}
	return tp
}

// toOllamaMessages flattens the system instruction + contents into Ollama chat
// messages, including assistant tool-calls and tool-result messages so the
// function-calling round-trip works.
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
		var text strings.Builder
		var toolCalls []api.ToolCall
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionResponse != nil:
				msgs = append(msgs, api.Message{
					Role:     "tool",
					ToolName: p.FunctionResponse.Name,
					Content:  jsonString(p.FunctionResponse.Response),
				})
			case p.FunctionCall != nil:
				toolCalls = append(toolCalls, toOllamaToolCall(p.FunctionCall))
			case p.Text != "":
				text.WriteString(p.Text)
			}
		}
		if text.Len() > 0 || len(toolCalls) > 0 {
			m := api.Message{Role: role, Content: text.String()}
			if len(toolCalls) > 0 {
				m.ToolCalls = toolCalls
			}
			msgs = append(msgs, m)
		}
	}
	return msgs
}

func toOllamaToolCall(fc *genai.FunctionCall) api.ToolCall {
	args := api.NewToolCallFunctionArguments()
	for k, v := range fc.Args {
		args.Set(k, v)
	}
	return api.ToolCall{Function: api.ToolCallFunction{Name: fc.Name, Arguments: args}}
}

func jsonString(v map[string]any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
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
