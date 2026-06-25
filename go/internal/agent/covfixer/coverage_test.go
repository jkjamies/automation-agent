package covfixer

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/model"

	"automation-agent/internal/agent/fixflow"
	"automation-agent/internal/agent/setup"
)

// scriptedLLM routes by the system prompt: triage, explore (plan), or execute (test).
type scriptedLLM struct{ triage, plan, test string }

func (s scriptedLLM) Name() string { return "scripted" }
func (s scriptedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	resp := s.test
	if req.Config != nil {
		sys := setup.ContentText(req.Config.SystemInstruction)
		switch {
		case strings.Contains(sys, "triaging"):
			resp = s.triage
		case strings.Contains(sys, "planning where to add"):
			resp = s.plan
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: setup.AssistantText(resp)}, nil)
	}
}

func TestParseTriage(t *testing.T) {
	work, err := parseTriage(`[{"path":"calc.go","uncovered":["Divide error path","Add edge cases"]},{"path":"","uncovered":[]}]`)
	if err != nil {
		t.Fatalf("parseTriage: %v", err)
	}
	if len(work) != 1 || work[0].Path != "calc.go" || len(work[0].Items) != 2 {
		t.Errorf("work = %+v", work)
	}
}

func TestTriage(t *testing.T) {
	work, err := Triage(context.Background(), scriptedLLM{triage: `[{"path":"calc.go","uncovered":["Divide"]}]`}, "jacoco xml")
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(work) != 1 || work[0].Path != "calc.go" {
		t.Errorf("work = %+v", work)
	}
	if _, err := Triage(context.Background(), scriptedLLM{triage: "[]"}, "report"); err == nil {
		t.Error("empty triage should error")
	}
}

func TestParsePlan(t *testing.T) {
	plan, err := parsePlan(`prose [{"source":"calc.go","test_path":"calc_test.go","framework":"go testing","notes":"package calc"},{"source":"","test_path":"x"}] more`)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan["calc.go"].TestPath != "calc_test.go" || plan["calc.go"].Framework != "go testing" {
		t.Errorf("entry = %+v", plan["calc.go"])
	}
}

func TestAnalyze(t *testing.T) {
	// A checkout with a source file and an existing test (so conventions are gathered).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\nfunc Divide(a,b int)(int,error){return a/b,nil}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "existing_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestExisting(t *testing.T){}"), 0o644); err != nil {
		t.Fatal(err)
	}

	llm := scriptedLLM{
		plan: `[{"source":"calc.go","test_path":"calc_test.go","framework":"go testing","notes":"package calc"}]`,
		test: "package calc\n\nimport \"testing\"\n\nfunc TestDivide(t *testing.T) {}\n",
	}
	in := fixflow.AnalyzeInput{LLM: llm, RepoDir: dir, Work: []fixflow.FileWork{{Path: "calc.go", Items: []string{"Divide"}}}}

	edits, err := Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(edits) != 1 || edits[0].Path != "calc_test.go" || !strings.Contains(edits[0].Content, "TestDivide") {
		t.Errorf("edits = %+v", edits)
	}
}

func TestBuildExecuteInput(t *testing.T) {
	got := buildExecuteInput(
		fixflow.FileWork{Path: "calc.go", Items: []string{"Divide"}},
		"package calc", planEntry{TestPath: "calc_test.go", Framework: "go testing", Notes: "pkg calc"}, "ci failed",
	)
	for _, w := range []string{"calc_test.go", "go testing", "pkg calc", "Divide", "package calc", "ci failed"} {
		if !strings.Contains(got, w) {
			t.Errorf("execute input missing %q", w)
		}
	}
}

func TestNewEngine(t *testing.T) {
	e := NewEngine(fixflow.Deps{})
	if e.CheckName() != "agent-coverage-verify" {
		t.Errorf("check=%q", e.CheckName())
	}
}

// TestLiveCoverage runs the full explore+execute against a real model (opt-in).
func TestLiveCoverage(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 to run the live coverage flow")
	}
	tag := os.Getenv("OLLAMA_MODEL")
	if tag == "" {
		tag = "gemma4:12b"
	}
	codeTag := os.Getenv("OLLAMA_CODE_MODEL")
	if codeTag == "" {
		codeTag = tag
	}
	llm, err := setup.NewOllamaModel("http://localhost:11434", tag)
	if err != nil {
		t.Fatal(err)
	}
	codeLLM, err := setup.NewOllamaModel("http://localhost:11434", codeTag)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("explore model=%s, code model=%s", tag, codeTag)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\n\nimport \"errors\"\n\nfunc Divide(a, b int) (int, error) {\n\tif b == 0 {\n\t\treturn 0, errors.New(\"divide by zero\")\n\t}\n\treturn a / b, nil\n}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "calc_existing_test.go"), []byte("package calc\n\nimport \"testing\"\n\nfunc TestExisting(t *testing.T) {}\n"), 0o644)

	in := fixflow.AnalyzeInput{LLM: llm, CodeLLM: codeLLM, RepoDir: dir, Work: []fixflow.FileWork{{Path: "calc.go", Items: []string{"Divide error path"}}}}
	edits, err := Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(edits) == 0 || strings.TrimSpace(edits[0].Content) == "" {
		t.Fatalf("no usable test edit: %+v", edits)
	}
	t.Logf("placed test at %s:\n%s", edits[0].Path, edits[0].Content)
}
