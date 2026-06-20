package setup

import (
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestGenerationOptions(t *testing.T) {
	opts := generationOptions(nil)
	if opts["temperature"] != 0.0 || opts["num_ctx"] != defaultNumCtx {
		t.Errorf("defaults = %+v", opts)
	}
	temp, top := float32(0.5), float32(0.9)
	var seed int32 = 7
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{Temperature: &temp, TopP: &top, Seed: &seed}}
	opts = generationOptions(req)
	if opts["temperature"] != 0.5 || opts["seed"] != 7 {
		t.Errorf("honored config = %+v", opts)
	}
	if tp, _ := opts["top_p"].(float64); tp < 0.89 || tp > 0.91 { // float32->float64 artifact
		t.Errorf("top_p = %v", opts["top_p"])
	}
}

func TestWantsJSON(t *testing.T) {
	if wantsJSON(nil) {
		t.Error("nil should not want json")
	}
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{ResponseMIMEType: "application/json"}}
	if !wantsJSON(req) {
		t.Error("application/json should want json")
	}
}

func TestToOllamaTools(t *testing.T) {
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "read_file",
			Description: "read a repo file",
			Parameters: &genai.Schema{
				Type:       genai.TypeObject,
				Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "the path"}},
				Required:   []string{"path"},
			},
		}},
	}}}}
	tools := toOllamaTools(req)
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	fn := tools[0].Function
	if fn.Name != "read_file" || tools[0].Type != "function" {
		t.Errorf("fn = %+v", fn)
	}
	if fn.Parameters.Type != "object" || len(fn.Parameters.Required) != 1 {
		t.Errorf("params = %+v", fn.Parameters)
	}
	prop, ok := fn.Parameters.Properties.Get("path")
	if !ok || len(prop.Type) != 1 || prop.Type[0] != "string" {
		t.Errorf("path prop = %+v (ok=%v) — type must be lowercased", prop, ok)
	}
	if toOllamaTools(&model.LLMRequest{}) != nil {
		t.Error("no config -> nil tools")
	}
}

func TestFinalResponseWithToolCalls(t *testing.T) {
	args := api.NewToolCallFunctionArguments()
	args.Set("city", "Paris")
	tc := api.ToolCall{Function: api.ToolCallFunction{Name: "get_weather", Arguments: args}}

	resp := finalResponse("here you go", []api.ToolCall{tc}, "gemma4")
	if !resp.TurnComplete || resp.FinishReason != genai.FinishReasonStop {
		t.Error("final response should be complete")
	}
	if len(resp.Content.Parts) != 2 {
		t.Fatalf("parts = %+v", resp.Content.Parts)
	}
	if resp.Content.Parts[0].Text != "here you go" {
		t.Errorf("text part = %q", resp.Content.Parts[0].Text)
	}
	fc := resp.Content.Parts[1].FunctionCall
	if fc == nil || fc.Name != "get_weather" || fc.Args["city"] != "Paris" {
		t.Errorf("function call = %+v", fc)
	}

	// no tool calls + no text -> empty parts, still complete
	if r := finalResponse("", nil, "gemma4"); len(r.Content.Parts) != 0 {
		t.Errorf("empty = %+v", r.Content.Parts)
	}
}

func TestToOllamaMessagesToolRoundTrip(t *testing.T) {
	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("sys", genai.RoleUser)},
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "read a.go"}}},
			{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "a.go"}}}}},
			{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{"content": "package a"}}}}},
		},
	}
	msgs := toOllamaMessages(req)
	if len(msgs) != 4 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Errorf("roles = %q %q", msgs[0].Role, msgs[1].Role)
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("assistant tool call = %+v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolName != "read_file" || !strings.Contains(msgs[3].Content, "package a") {
		t.Errorf("tool result msg = %+v", msgs[3])
	}
}

func TestToGenaiFunctionCall(t *testing.T) {
	args := api.NewToolCallFunctionArguments()
	args.Set("x", "y")
	fc := toGenaiFunctionCall(api.ToolCall{Function: api.ToolCallFunction{Name: "f", Arguments: args}})
	if fc.Name != "f" || fc.Args["x"] != "y" {
		t.Errorf("fc = %+v", fc)
	}
}

func TestJSONString(t *testing.T) {
	if jsonString(nil) != "" {
		t.Error("nil -> empty")
	}
	if got := jsonString(map[string]any{"a": "b"}); got != `{"a":"b"}` {
		t.Errorf("got %q", got)
	}
}
