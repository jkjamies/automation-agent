package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticketflow/internal/fakellm"
	"ticketflow/internal/gate"
)

func task(t *testing.T) Task {
	return Task{
		Dir: t.TempDir(), Key: "PROJ-1", Summary: "Add x", Description: "Do {not} template this.",
		Plan: "Write x.txt.", Checks: []gate.Check{{Name: "test", Run: []string{"true"}}}, Attempt: 2, Feedback: "it failed",
	}
}

func TestPrompt(t *testing.T) {
	p, err := task(t).Prompt()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PROJ-1", "attempt 2", "Do {not} template this.", "Write x.txt.", "test", "it failed"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestADKHarnessUsesTools(t *testing.T) {
	tk := task(t)
	m := &fakellm.Model{
		ModelName: "fake-coder",
		Replies:   []string{"Wrote x.txt."},
		ToolCall:  &fakellm.Call{Name: "write_file", Args: map[string]any{"path": "x.txt", "content": "hello\n"}},
	}
	h := &ADK{Model: m, CheckTimeout: time.Second}
	res, err := h.Implement(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(tk.Dir, "x.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("x.txt = %q, %v; the tool call did not reach the checkout", b, err)
	}
	if res.Summary == "" || res.Activity != 2 {
		t.Errorf("result = %+v, want a summary after 2 model calls", res)
	}
	if h.Name() != "adk:fake-coder" {
		t.Errorf("Name = %q", h.Name())
	}
}

func TestADKHarnessLoadsRepoSkills(t *testing.T) {
	tk := task(t)
	skill := filepath.Join(tk.Dir, ".agents", "skills", "add-endpoint")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: add-endpoint\ndescription: How to add an HTTP endpoint here.\n---\n\nSteps.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := skillToolset(context.Background(), filepath.Join(tk.Dir, ".agents", "skills"), "s")
	if err != nil || ts == nil {
		t.Fatalf("skillToolset = %v, %v", ts, err)
	}
	if ts, err := skillToolset(context.Background(), filepath.Join(tk.Dir, "absent"), "s"); ts != nil || err != nil {
		t.Errorf("absent dir = %v, %v; want nil, nil", ts, err)
	}
	h := &ADK{Model: &fakellm.Model{Replies: []string{"done"}}}
	if _, err := h.Implement(context.Background(), tk); err != nil {
		t.Errorf("Implement with skills: %v", err)
	}
}

func TestADKHarnessBudget(t *testing.T) {
	m := &fakellm.Model{Replies: []string{"never"}, ToolCall: &fakellm.Call{Name: "list_dir", Args: map[string]any{"path": "."}}}
	_, err := (&ADK{Model: m, MaxModelCalls: 1}).Implement(context.Background(), task(t))
	if !errors.Is(err, ErrBudget) {
		t.Errorf("err = %v, want ErrBudget", err)
	}
}

func TestCLIHarness(t *testing.T) {
	tk := task(t)
	h := &CLI{
		Argv: []string{"sh", "-c", `cp "$1" task-copy.md && echo "dir=$2 env=$FOO token=${GITHUB_TOKEN:-none}"`, "sh", "{prompt_file}", "{workdir}"},
		Env:  []string{"FOO=bar"},
	}
	t.Setenv("GITHUB_TOKEN", "leak")
	res, err := h.Implement(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "dir="+tk.Dir) || !strings.Contains(res.Summary, "env=bar") || !strings.Contains(res.Summary, "token=none") {
		t.Errorf("summary = %q", res.Summary)
	}
	b, err := os.ReadFile(filepath.Join(tk.Dir, "task-copy.md"))
	if err != nil || !strings.Contains(string(b), "PROJ-1") {
		t.Errorf("task file not handed over: %q, %v", b, err)
	}
	if h.Name() != "cli:sh" {
		t.Errorf("Name = %q", h.Name())
	}
}

func TestCLIHarnessFailures(t *testing.T) {
	tk := task(t)
	if _, err := (&CLI{}).Implement(context.Background(), tk); err == nil {
		t.Error("empty argv succeeded")
	}
	if _, err := (&CLI{Argv: []string{"sh", "-c", "exit 4"}}).Implement(context.Background(), tk); err == nil || !strings.Contains(err.Error(), "exited 4") {
		t.Errorf("err = %v, want exit 4", err)
	}
	if _, err := (&CLI{Argv: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond}).Implement(context.Background(), tk); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want timeout", err)
	}
	if _, err := (&CLI{Argv: []string{"/does/not/exist"}}).Implement(context.Background(), tk); err == nil {
		t.Error("missing binary succeeded")
	}
	if (&CLI{}).Name() != "cli" {
		t.Error("empty CLI name")
	}
}
