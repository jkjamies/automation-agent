package summary

import (
	"embed"
	"fmt"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/notify"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = setup.NewPrompts(promptFS)

// Deps are the injected dependencies for the summary workflow.
type Deps struct {
	LLM    model.LLM
	GH     CommitLister
	Notify notify.Notifier
	Repos  []string         // owner/repo entries; one parallel fetcher each
	Window time.Duration    // commit window; defaults to 24h
	Title  string           // digest notification title; defaults to "Daily commit digest"
	Now    func() time.Time // injectable clock; defaults to time.Now
}

// BuildSummaryAgent wires the summary workflow:
//
//	Sequential[ Parallel[fetch×N] -> summarize(LLM) -> notify ]
//
// Fetchers write per-repo commit data to state; the summarizer reads it via its
// instruction provider and writes the digest; the notifier posts it.
func BuildSummaryAgent(d Deps) (agent.Agent, error) {
	if len(d.Repos) == 0 {
		return nil, fmt.Errorf("summary: at least one repo is required")
	}
	if d.LLM == nil || d.GH == nil || d.Notify == nil {
		return nil, fmt.Errorf("summary: LLM, GH and Notify are required")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	window := d.Window
	if window == 0 {
		window = 24 * time.Hour
	}
	title := d.Title
	if title == "" {
		title = "Daily commit digest"
	}

	fetchers := make([]agent.Agent, 0, len(d.Repos))
	for _, repo := range d.Repos {
		fetcher, err := newFetchAgent(repo, d.GH, window, now)
		if err != nil {
			return nil, fmt.Errorf("build fetcher for %s: %w", repo, err)
		}
		fetchers = append(fetchers, fetcher)
	}
	parallel, err := parallelagent.New(parallelagent.Config{AgentConfig: agent.Config{
		Name:        "fetch_all",
		Description: "Fetches recent commits for all configured repositories",
		SubAgents:   fetchers,
	}})
	if err != nil {
		return nil, fmt.Errorf("build parallel fetchers: %w", err)
	}

	summarizer, err := llmagent.New(llmagent.Config{
		Name:                "summarizer",
		Description:         "Summarizes recent commits into a digest",
		Model:               d.LLM,
		InstructionProvider: summaryInstruction(prompts.MustGet("summarize")),
		OutputKey:           digestKey,
	})
	if err != nil {
		return nil, fmt.Errorf("build summarizer: %w", err)
	}

	notifier, err := newNotifyAgent(d.Notify, title)
	if err != nil {
		return nil, fmt.Errorf("build notifier: %w", err)
	}

	return sequentialagent.New(sequentialagent.Config{AgentConfig: agent.Config{
		Name:        "summary_workflow",
		Description: "Commit digest workflow",
		SubAgents:   []agent.Agent{parallel, summarizer, notifier},
	}})
}
