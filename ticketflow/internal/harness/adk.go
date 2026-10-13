package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"ticketflow/internal/prompts"
	"ticketflow/internal/repotools"
)

// ADK implements the task with an in-process ADK agent. It sees the checkout only through
// repotools (confined paths, no shell, only the repository's named checks) and, when the
// repository ships Agent Skills, through ADK's skill toolset — the same SKILL.md files a
// developer's Antigravity session would load, so humans and the pipeline share playbooks.
type ADK struct {
	Model         model.LLM
	MaxModelCalls int           // budget per attempt; zero means 150
	CheckTimeout  time.Duration // per run_check call
	Log           *slog.Logger
}

// SkillDirs are the repository directories searched for Agent Skills, in order.
var SkillDirs = []string{".agents/skills", ".agent/skills", ".gemini/skills"}

// ErrBudget is returned when an attempt exhausts its model-call budget.
var ErrBudget = errors.New("harness: model-call budget exhausted")

// Name implements Harness.
func (h *ADK) Name() string { return "adk:" + h.Model.Name() }

// Implement implements Harness.
func (h *ADK) Implement(ctx context.Context, t Task) (Result, error) {
	start := time.Now()
	budget := h.MaxModelCalls
	if budget <= 0 {
		budget = 150
	}
	log := h.Log
	if log == nil {
		log = slog.Default()
	}

	tools, err := repotools.ReadWrite(t.Dir, t.Checks, h.CheckTimeout)
	if err != nil {
		return Result{}, fmt.Errorf("harness: build tools: %w", err)
	}
	var toolsets []tool.Toolset
	for i, dir := range SkillDirs {
		ts, err := skillToolset(ctx, filepath.Join(t.Dir, dir), fmt.Sprintf("repo_skills_%d", i))
		if err != nil {
			log.WarnContext(ctx, "skipping repository skills", "dir", dir, "err", err)
			continue
		}
		if ts != nil {
			toolsets = append(toolsets, ts)
		}
	}

	var calls atomic.Int64
	coder, err := llmagent.New(llmagent.Config{
		Name:                "coder",
		Description:         "Implements a Jira ticket in a sandboxed checkout.",
		Model:               h.Model,
		InstructionProvider: Static(prompts.Get("implementer.md")),
		Tools:               tools,
		Toolsets:            toolsets,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(agent.Context, *model.LLMRequest) (*model.LLMResponse, error) {
				if calls.Add(1) > int64(budget) {
					return nil, fmt.Errorf("%w (%d calls)", ErrBudget, budget)
				}
				return nil, nil
			},
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("harness: build coder: %w", err)
	}
	msg, err := t.Prompt()
	if err != nil {
		return Result{}, err
	}
	text, err := RunAgent(ctx, coder, msg)
	return Result{Summary: text, Elapsed: time.Since(start), Activity: int(calls.Load())}, err
}

// skillToolset loads Agent Skills from dir, returning nil when the directory is absent.
func skillToolset(ctx context.Context, dir, name string) (tool.Toolset, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, nil
	}
	src := skill.NewFileSystemSource(os.DirFS(dir))
	fms, err := src.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	if len(fms) == 0 {
		return nil, nil
	}
	return skilltoolset.New(ctx, skilltoolset.Config{Source: src, Name: name})
}

// Static is an InstructionProvider returning s verbatim. ADK templates the plain
// Instruction field ({name} is a state lookup); text the service did not author must
// bypass that.
func Static(s string) llmagent.InstructionProvider {
	return func(agent.ReadonlyContext) (string, error) { return s, nil }
}

// RunAgent drives a one-shot agent over a fresh in-memory session and returns the text
// of its final response.
func RunAgent(ctx context.Context, a agent.Agent, message string) (string, error) {
	r, err := runner.New(runner.Config{
		AppName:           "ticketflow",
		Agent:             a,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return "", fmt.Errorf("harness: build runner: %w", err)
	}
	var final string
	msg := genai.NewContentFromText(message, genai.RoleUser)
	for ev, err := range r.Run(ctx, "ticketflow", "run", msg, agent.RunConfig{}) {
		if err != nil {
			return final, err
		}
		if ev == nil || ev.Content == nil || ev.Content.Role != genai.RoleModel {
			continue
		}
		var b strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" && !p.Thought {
				b.WriteString(p.Text)
			}
		}
		if b.Len() > 0 {
			final = b.String()
		}
	}
	return strings.TrimSpace(final), nil
}
