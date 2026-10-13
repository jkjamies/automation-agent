// Package fakellm is a scripted model.LLM for tests: deterministic, offline, and able to
// drive an agent through a tool call so tool wiring is exercised for real. Tests use it to
// test routing and plumbing, never to assert on what a real model would say.
package fakellm

import (
	"context"
	"iter"
	"strconv"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Call is a tool call the model makes before answering.
type Call struct {
	Name string
	Args map[string]any
}

// Model replies with Replies in order (repeating the last once exhausted). When ToolCall
// is set, each turn first calls that tool and answers only after seeing its response.
type Model struct {
	ModelName string
	Replies   []string
	ToolCall  *Call

	mu        sync.Mutex
	turns     int
	toolSeen  []map[string]any
	callCount int
}

// Name implements model.LLM.
func (m *Model) Name() string {
	if m.ModelName == "" {
		return "fake"
	}
	return m.ModelName
}

// Turns is how many final answers the model has given.
func (m *Model) Turns() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns
}

// ToolResponses are the tool results the model was shown, in order.
func (m *Model) ToolResponses() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.toolSeen...)
}

// GenerateContent implements model.LLM.
func (m *Model) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		var resp *genai.Content
		if fr := lastFunctionResponse(req); m.ToolCall != nil && fr == nil {
			m.callCount++
			resp = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "call-" + strconv.Itoa(m.callCount), Name: m.ToolCall.Name, Args: m.ToolCall.Args,
			}}}}
		} else {
			if fr != nil {
				m.toolSeen = append(m.toolSeen, fr.Response)
			}
			text := ""
			if len(m.Replies) > 0 {
				i := m.turns
				if i >= len(m.Replies) {
					i = len(m.Replies) - 1
				}
				text = m.Replies[i]
			}
			m.turns++
			resp = genai.NewContentFromText(text, genai.RoleModel)
		}
		m.mu.Unlock()
		yield(&model.LLMResponse{Content: resp, TurnComplete: true, FinishReason: genai.FinishReasonStop}, nil)
	}
}

// lastFunctionResponse returns the function response in the newest request content, if
// that content is one — i.e. the model is being shown the result of its own tool call.
func lastFunctionResponse(req *model.LLMRequest) *genai.FunctionResponse {
	if req == nil || len(req.Contents) == 0 {
		return nil
	}
	last := req.Contents[len(req.Contents)-1]
	if last == nil {
		return nil
	}
	for _, p := range last.Parts {
		if p != nil && p.FunctionResponse != nil {
			return p.FunctionResponse
		}
	}
	return nil
}
