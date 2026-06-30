package reviewer

import (
	"embed"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"

	"automation-agent/internal/agent/setup"
)

// promptFS embeds the category, glue, and distiller prompts as reviewable markdown next to the
// agent (the prompts-as-markdown convention).
//
//go:embed prompts/*.md
var promptFS embed.FS

var prompts = setup.NewPrompts(promptFS)

// distillTrigger kicks the distiller drive; the real instruction (the distill prompt + the repo's
// standards docs) lives in the agent's system instruction.
const distillTrigger = "Extract the repository's rules as the JSON array specified."

// buildCategoryAgent builds one category review agent: an LLM agent on the category's tier whose
// instruction is the category prompt + the repo's standards rule menu (when any) + the filtered
// diff, writing its findings JSON to the category's state key. When standards are present it also
// gets the lazy get_rule tool. The diff/standards are baked into the instruction because they are
// per-event.
func (e *Engine) buildCategoryAgent(c category, diff string, std *standards) (agent.Agent, error) {
	body, err := prompts.Get(c.promptName)
	if err != nil {
		return nil, fmt.Errorf("reviewer: load %s prompt: %w", c.name, err)
	}
	tools, err := standardsTools(std)
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:                  "review_" + c.name,
		Description:           c.title + " review",
		Model:                 e.modelForTier(c.tier),
		Instruction:           buildReviewInstruction(body, diff, std),
		Tools:                 tools,
		OutputKey:             findingsKey(c.name),
		GenerateContentConfig: setup.JSONConfig(),
	})
}

// buildGlueAgent builds the glue/synthesis agent: a code-tier LLM agent that sees the diff, the
// category findings so far, and the repo's standards rule menu, emitting additional architectural-
// alignment / testability / test-coverage findings (cross-lens dedup is done deterministically in
// code, not here).
func (e *Engine) buildGlueAgent(diff string, prior []Finding, std *standards) (agent.Agent, error) {
	body, err := prompts.Get("glue")
	if err != nil {
		return nil, fmt.Errorf("reviewer: load glue prompt: %w", err)
	}
	tools, err := standardsTools(std)
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:                  "review_glue",
		Description:           "Holistic synthesis review",
		Model:                 e.codeLLM,
		Instruction:           buildGlueInstruction(body, diff, prior, std),
		Tools:                 tools,
		GenerateContentConfig: setup.JSONConfig(),
	})
}

// buildDistillerAgent builds the standards distiller: a base-tier LLM agent (distillation is
// summarization/extraction, the base tier per model-size-split) fed the reviewed repo's standards
// docs, prompted to emit a uniform tagged rule list. It normalizes heterogeneous formats
// (.agents/standards, .cursor/rules, CLAUDE.md, …) into one list.
func (e *Engine) buildDistillerAgent(docs map[string]string, sources []string) (agent.Agent, error) {
	body, err := prompts.Get("distill")
	if err != nil {
		return nil, fmt.Errorf("reviewer: load distill prompt: %w", err)
	}
	return llmagent.New(llmagent.Config{
		Name:                  "standards_distiller",
		Description:           "Distill the repo's standards docs into a tagged rule list",
		Model:                 e.baseLLM,
		Instruction:           buildDistillerInstruction(body, docs, sources),
		GenerateContentConfig: setup.JSONConfig(),
	})
}
