package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// fakeOllama serves the given chunks as newline-delimited JSON from /api/chat,
// mimicking the real Ollama server.
func fakeOllama(t *testing.T, chunks []api.ChatResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, c := range chunks {
			if err := enc.Encode(c); err != nil {
				t.Fatalf("encode chunk: %v", err)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}

func collect(t *testing.T, seq func(yield func(*model.LLMResponse, error) bool)) []*model.LLMResponse {
	t.Helper()
	var out []*model.LLMResponse
	for resp, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		out = append(out, resp)
	}
	return out
}

func TestOllamaStreaming(t *testing.T) {
	ts := fakeOllama(t, []api.ChatResponse{
		{Model: "gemma", Message: api.Message{Role: "assistant", Content: "Hello "}, Done: false},
		{Model: "gemma", Message: api.Message{Role: "assistant", Content: "world"}, Done: true, DoneReason: "stop"},
	})
	defer ts.Close()

	m, err := NewOllamaModel(ts.URL, "gemma")
	if err != nil {
		t.Fatalf("NewOllamaModel: %v", err)
	}

	req := &model.LLMRequest{Contents: []*genai.Content{UserText("hi")}}
	resps := collect(t, m.GenerateContent(context.Background(), req, true))

	var partials []string
	var final *model.LLMResponse
	for _, r := range resps {
		if r.Partial {
			partials = append(partials, ContentText(r.Content))
		}
		if r.TurnComplete {
			final = r
		}
	}
	if len(partials) != 1 || partials[0] != "Hello " {
		t.Errorf("partials = %v, want [\"Hello \"]", partials)
	}
	if final == nil {
		t.Fatal("no final response")
	}
	if got := ContentText(final.Content); got != "Hello world" {
		t.Errorf("final content = %q, want %q", got, "Hello world")
	}
	if final.FinishReason != genai.FinishReasonStop {
		t.Errorf("finish reason = %q", final.FinishReason)
	}
}

func TestOllamaNonStreaming(t *testing.T) {
	ts := fakeOllama(t, []api.ChatResponse{
		{Model: "gemma", Message: api.Message{Role: "assistant", Content: "Full answer"}, Done: true, DoneReason: "stop"},
	})
	defer ts.Close()

	m, _ := NewOllamaModel(ts.URL, "gemma")
	req := &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{SystemInstruction: UserText("be brief")},
		Contents: []*genai.Content{UserText("hi")},
	}
	resps := collect(t, m.GenerateContent(context.Background(), req, false))

	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if got := ContentText(resps[0].Content); got != "Full answer" {
		t.Errorf("content = %q", got)
	}
	if !resps[0].TurnComplete {
		t.Error("non-streaming response should be TurnComplete")
	}
}

func TestOllamaServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	m, _ := NewOllamaModel(ts.URL, "gemma")
	var gotErr error
	for _, err := range m.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("expected an error from a 500 response")
	}
}

func TestNewOllamaModelEmptyTag(t *testing.T) {
	if _, err := NewOllamaModel("http://localhost:11434", ""); err == nil {
		t.Fatal("expected error for empty model tag")
	}
}

func TestToOllamaMessages(t *testing.T) {
	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{SystemInstruction: UserText("system rules")},
		Contents: []*genai.Content{
			UserText("question"),
			genai.NewContentFromText("answer", genai.RoleModel),
			nil, // must be skipped
		},
	}
	msgs := toOllamaMessages(req)
	want := []api.Message{
		{Role: "system", Content: "system rules"},
		{Role: "user", Content: "question"},
		{Role: "assistant", Content: "answer"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i].Role != want[i].Role || msgs[i].Content != want[i].Content {
			t.Errorf("msg[%d] = {%s, %q}, want {%s, %q}", i, msgs[i].Role, msgs[i].Content, want[i].Role, want[i].Content)
		}
	}
}

// TestLiveOllama is an opt-in smoke test against a real Ollama server. It is
// skipped unless OLLAMA_LIVE=1. It asserts only that a non-empty response comes
// back (never on content), so it stays out of CI and isn't flaky.
func TestLiveOllama(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 to run against a real Ollama server")
	}
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	tag := os.Getenv("OLLAMA_MODEL")
	if tag == "" {
		tag = "gemma4:e4b"
	}

	m, err := NewOllamaModel(host, tag)
	if err != nil {
		t.Fatalf("NewOllamaModel: %v", err)
	}
	req := &model.LLMRequest{Contents: []*genai.Content{UserText("Reply with the single word: pong")}}

	var sb strings.Builder
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("live generate: %v", err)
		}
		sb.WriteString(ContentText(resp.Content))
	}
	if strings.TrimSpace(sb.String()) == "" {
		t.Fatal("empty response from live model")
	}
	t.Logf("live %s replied: %q", tag, strings.TrimSpace(sb.String()))
}

func TestModelNameOverride(t *testing.T) {
	m, _ := NewOllamaModel("http://localhost:11434", "default-model")
	if got := m.modelName(&model.LLMRequest{Model: "override"}); got != "override" {
		t.Errorf("modelName = %q, want override", got)
	}
	if got := m.modelName(&model.LLMRequest{}); got != "default-model" {
		t.Errorf("modelName = %q, want default-model", got)
	}
}
