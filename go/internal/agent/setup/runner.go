package setup

import (
	"context"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
)

// NewRunner builds an in-memory runner rooted at root, suitable for ephemeral one-shot
// drives (explore/analyze/triage) that complete within a single invocation and never
// need to survive a restart.
func NewRunner(appName string, root agent.Agent) (*runner.Runner, error) {
	return newRunner(appName, root, session.InMemoryService())
}

// newRunner builds a runner over the given session service. A durable service
// (sqlite/firestore) lets a parked long-running run resume after a process restart.
func newRunner(appName string, root agent.Agent, sess session.Service) (*runner.Runner, error) {
	return runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    sess,
		AutoCreateSession: true,
	})
}

// streamingRunConfig is the single run configuration every drive uses. SSE streaming
// is required: with it, Ollama flushes response headers and the first chunk right after
// model-load + prefill, and the (potentially long) token-by-token decode streams over a
// long-lived body with no overall timeout — so a slow generation on modest hardware never
// trips the transport's first-chunk timeout. Without streaming, Ollama buffers the whole
// answer before sending any bytes, turning that timeout into a cap on total generation.
// All event consumers below already ignore partial events, so streaming is transparent.
func streamingRunConfig() agent.RunConfig {
	return agent.RunConfig{StreamingMode: agent.StreamingModeSSE}
}

// Drive runs the agent for a single input, draining events and returning the first
// error. Side-effecting agents (e.g. a notifier) perform their work as they run.
func Drive(ctx context.Context, r *runner.Runner, userID, sessionID, input string) error {
	for _, err := range r.Run(ctx, userID, sessionID, userText(input), streamingRunConfig()) {
		if err != nil {
			return err
		}
	}
	return nil
}

// AgentStats is what a drive observed about one agent, keyed by agent name in RunReport.Agents.
type AgentStats struct {
	// Elapsed is wall time from the start of the drive to the agent's last event. For agents
	// run in parallel that includes any wait for a model concurrency slot — it is how long the
	// agent took as the caller experienced it, not model time.
	Elapsed time.Duration
	// TokensIn and TokensOut sum the prompt and candidate token counts the model reported across
	// the agent's non-partial responses. Reported is false when it reported none, so a caller
	// can tell "0 tokens" from "unknown" (a test double, or an adapter with no usage data).
	TokensIn, TokensOut int
	Reported            bool
	// Model is the model version the agent's last response named, or "" when the adapter
	// reports none.
	Model string
}

// RunReport is everything a drive collected: the merged state deltas, the concatenated text of
// the non-partial responses, and per-agent statistics.
type RunReport struct {
	State  map[string]any
	Text   string
	Agents map[string]AgentStats
}

// DriveReport runs the agent for a single input and returns a RunReport. It is the one event
// loop behind DriveText and DriveCollectState; use it directly when the caller wants more than
// one of the report's parts — a fan-out that reads each sub-agent's state key and also reports
// how long and how many tokens each took.
func DriveReport(ctx context.Context, r *runner.Runner, userID, sessionID, input string) (RunReport, error) {
	rep := RunReport{State: make(map[string]any), Agents: make(map[string]AgentStats)}
	var text strings.Builder
	start := time.Now()
	for ev, err := range r.Run(ctx, userID, sessionID, userText(input), streamingRunConfig()) {
		if err != nil {
			return RunReport{}, err
		}
		if ev.Partial {
			continue // partial chunks carry neither state nor final usage; the final event does
		}
		if ev.Content != nil {
			text.WriteString(contentText(ev.Content))
		}
		for k, v := range ev.Actions.StateDelta {
			rep.State[k] = v
		}
		st := rep.Agents[ev.Author]
		st.Elapsed = time.Since(start)
		if u := ev.UsageMetadata; u != nil {
			st.Reported = true
			st.TokensIn += int(u.PromptTokenCount)
			st.TokensOut += int(u.CandidatesTokenCount)
		}
		if ev.ModelVersion != "" {
			st.Model = ev.ModelVersion
		}
		rep.Agents[ev.Author] = st
	}
	rep.Text = text.String()
	return rep, nil
}

// DriveText runs the agent and returns the concatenated text of its non-partial
// responses. For a tool-using agent this is the final answer after any tool calls
// (intermediate function-call/response events carry no text).
func DriveText(ctx context.Context, r *runner.Runner, userID, sessionID, input string) (string, error) {
	rep, err := DriveReport(ctx, r, userID, sessionID, input)
	if err != nil {
		return "", err
	}
	return rep.Text, nil
}

// DriveCollectState runs the agent and accumulates every state delta emitted by its
// events into a single map. Useful for fan-out workflows where parallel sub-agents
// each write a distinct state key the caller needs to read back.
func DriveCollectState(ctx context.Context, r *runner.Runner, userID, sessionID, input string) (map[string]any, error) {
	rep, err := DriveReport(ctx, r, userID, sessionID, input)
	if err != nil {
		return nil, err
	}
	return rep.State, nil
}
