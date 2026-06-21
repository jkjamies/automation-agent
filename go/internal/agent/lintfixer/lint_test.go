package lintfixer

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/model"

	"github.com/jkjamies/automation-agent/internal/agent/fixflow"
	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

type stubLLM struct{ text string }

func (s stubLLM) Name() string { return "stub" }
func (s stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: setup.AssistantText(s.text)}, nil)
	}
}

func TestParseTriage(t *testing.T) {
	work, err := parseTriage(`x [{"path":"a.go","problems":["unchecked error"]},{"path":"","problems":[]}] y`)
	if err != nil {
		t.Fatalf("parseTriage: %v", err)
	}
	if len(work) != 1 || work[0].Path != "a.go" || len(work[0].Items) != 1 {
		t.Errorf("work = %+v", work)
	}
}

func TestTriage(t *testing.T) {
	work, err := Triage(context.Background(), stubLLM{`[{"path":"a.go","problems":["x"]}]`}, "report")
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(work) != 1 || work[0].Path != "a.go" {
		t.Errorf("work = %+v", work)
	}
	if _, err := Triage(context.Background(), stubLLM{"[]"}, "report"); err == nil {
		t.Error("empty triage should error")
	}
}

func TestBuildFilePrompt(t *testing.T) {
	p := buildFilePrompt(fixflow.FileWork{Path: "a.go", Items: []string{"unchecked error"}}, "package a", "ci failed")
	for _, want := range []string{"a.go", "unchecked error", "package a", "ci failed"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestAnalyze(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := fixflow.AnalyzeInput{
		LLM:     stubLLM{"package fixed\n"},
		RepoDir: dir,
		Work:    []fixflow.FileWork{{Path: "a.go", Items: []string{"x"}}},
	}
	edits, err := Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(edits) != 1 || edits[0].Path != "a.go" || edits[0].Content != "package fixed\n" {
		t.Errorf("edits = %+v", edits)
	}
}

func TestNewEngine(t *testing.T) {
	e := NewEngine(fixflow.Deps{})
	if e.CheckName() != "agent-lint-verify" || e.Label() != "automation-agent" {
		t.Errorf("check=%q label=%q", e.CheckName(), e.Label())
	}
}
