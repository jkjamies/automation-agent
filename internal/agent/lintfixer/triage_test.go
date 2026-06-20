package lintfixer

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

func TestExtractJSONArray(t *testing.T) {
	if got := extractJSONArray("noise [1,2] more"); got != "[1,2]" {
		t.Errorf("got %q", got)
	}
	if got := extractJSONArray("no array here"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestParseTriage(t *testing.T) {
	files, err := parseTriage("```json\n[{\"path\":\"a.go\",\"problems\":[\"x\"]},{\"path\":\"\",\"problems\":[]}]\n```")
	if err != nil {
		t.Fatalf("parseTriage: %v", err)
	}
	if len(files) != 1 || files[0].Path != "a.go" {
		t.Errorf("files = %+v (empty path should be dropped)", files)
	}
}

func TestParseTriageNoArray(t *testing.T) {
	if _, err := parseTriage("the model rambled with no json"); err == nil {
		t.Fatal("expected error when no JSON array present")
	}
}

func TestTriageWithStub(t *testing.T) {
	files, err := Triage(context.Background(), stubLLM{text: `[{"path":"internal/foo.go","problems":["unchecked error"]}]`}, "raw report")
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(files) != 1 || files[0].Path != "internal/foo.go" {
		t.Errorf("files = %+v", files)
	}
}

func TestTriageEmpty(t *testing.T) {
	if _, err := Triage(context.Background(), stubLLM{text: "[]"}, "report"); err == nil {
		t.Fatal("empty triage should error (nothing to fix)")
	}
}

// TestLiveTriage normalizes a real report with a real model (opt-in via OLLAMA_LIVE).
func TestLiveTriage(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 to run the live triage")
	}
	tag := os.Getenv("OLLAMA_MODEL")
	if tag == "" {
		tag = "gemma4:e4b"
	}
	llm, err := setup.NewOllamaModel("http://localhost:11434", tag)
	if err != nil {
		t.Fatal(err)
	}
	report := `{"issues":[{"file":"internal/foo.go","line":10,"rule":"errcheck","message":"Error return value not checked"}]}`

	files, err := Triage(context.Background(), llm, report)
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected at least one file")
	}
	if !strings.Contains(files[0].Path, "foo.go") {
		t.Errorf("path = %q, expected to mention foo.go", files[0].Path)
	}
	t.Logf("triaged: %+v", files)
}
