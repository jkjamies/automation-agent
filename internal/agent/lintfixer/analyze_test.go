package lintfixer

import (
	"context"
	"iter"
	"os"
	"strings"
	"testing"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/lintfixer/models"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

// stubLLM returns a fixed text regardless of input — no genai import needed.
type stubLLM struct{ text string }

func (s stubLLM) Name() string { return "stub" }
func (s stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: setup.AssistantText(s.text)}, nil)
	}
}

func TestParseFileContent(t *testing.T) {
	if got := parseFileContent("```go\npackage x\n```"); got != "package x\n" {
		t.Errorf("fenced = %q", got)
	}
	if got := parseFileContent("package y"); got != "package y\n" {
		t.Errorf("plain = %q", got)
	}
}

func TestBuildFilePrompt(t *testing.T) {
	p := buildFilePrompt("a.go", "package a", []string{"unchecked error"}, "ci failed: build broke")
	for _, want := range []string{"a.go", "unchecked error", "package a", "ci failed: build broke"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestRunAnalyzeWithStub(t *testing.T) {
	files := []models.FileProblems{
		{Path: "b.go", Problems: []string{"y"}},
		{Path: "a.go", Problems: []string{"x"}},
	}
	contents := map[string]string{"a.go": "package a", "b.go": "package b"}

	edits, err := RunAnalyze(context.Background(), stubLLM{text: "package fixed\n"}, files, contents, "")
	if err != nil {
		t.Fatalf("RunAnalyze: %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("edits = %d, want 2", len(edits))
	}
	if edits[0].Path != "a.go" || edits[1].Path != "b.go" {
		t.Errorf("edits not sorted by path: %+v", edits)
	}
	if edits[0].Content != "package fixed\n" {
		t.Errorf("content = %q", edits[0].Content)
	}
}

func TestRunAnalyzeNoFiles(t *testing.T) {
	if _, err := RunAnalyze(context.Background(), stubLLM{}, nil, nil, ""); err == nil {
		t.Fatal("expected error with no files")
	}
}

// TestLiveAnalyze fixes a real file with a real model (opt-in via OLLAMA_LIVE),
// asserting a non-empty edit comes back.
func TestLiveAnalyze(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 to run the live analyze")
	}
	tag := os.Getenv("OLLAMA_MODEL")
	if tag == "" {
		tag = "gemma4:e4b"
	}
	llm, err := setup.NewOllamaModel("http://localhost:11434", tag)
	if err != nil {
		t.Fatal(err)
	}
	files := []models.FileProblems{{Path: "main.go", Problems: []string{"fmt imported but not used"}}}
	contents := map[string]string{"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() {}\n"}

	edits, err := RunAnalyze(context.Background(), llm, files, contents, "")
	if err != nil {
		t.Fatalf("RunAnalyze: %v", err)
	}
	if len(edits) != 1 || strings.TrimSpace(edits[0].Content) == "" {
		t.Fatalf("unexpected edits: %+v", edits)
	}
	t.Logf("fixed main.go:\n%s", edits[0].Content)
}
