package reviewer

import (
	"embed"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"

	"automation-agent/internal/agent/setup"
)

// promptFS embeds the category and glue prompts as reviewable markdown next to the agent
// (the prompts-as-markdown convention).
//
//go:embed prompts/*.md
var promptFS embed.FS

var prompts = setup.NewPrompts(promptFS)

// buildCategoryAgent builds one category review agent: an LLM agent on the category's tier
// whose instruction is the category prompt plus the filtered diff, writing its findings JSON
// to the category's state key. The diff is baked into the instruction because it is per-event.
func (e *Engine) buildCategoryAgent(c category, diff string) (agent.Agent, error) {
	body, err := prompts.Get(c.promptName)
	if err != nil {
		return nil, fmt.Errorf("reviewer: load %s prompt: %w", c.name, err)
	}
	return llmagent.New(llmagent.Config{
		Name:                  "review_" + c.name,
		Description:           c.title + " review",
		Model:                 e.modelForTier(c.tier),
		Instruction:           buildReviewInstruction(body, diff),
		OutputKey:             findingsKey(c.name),
		GenerateContentConfig: setup.JSONConfig(),
	})
}

// buildGlueAgent builds the glue/synthesis agent: a code-tier LLM agent that sees the diff and
// the category findings so far and emits additional architectural-alignment / testability /
// test-coverage findings (cross-lens dedup is done deterministically in code, not here).
func (e *Engine) buildGlueAgent(diff string, prior []Finding) (agent.Agent, error) {
	body, err := prompts.Get("glue")
	if err != nil {
		return nil, fmt.Errorf("reviewer: load glue prompt: %w", err)
	}
	return llmagent.New(llmagent.Config{
		Name:                  "review_glue",
		Description:           "Holistic synthesis review",
		Model:                 e.codeLLM,
		Instruction:           buildGlueInstruction(body, diff, prior),
		GenerateContentConfig: setup.JSONConfig(),
	})
}
