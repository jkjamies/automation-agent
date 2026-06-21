package setup

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/genai"
)

// DriveResult is the outcome of driving a long-running agent through one cycle: until it
// suspends on a long-running tool call, or finishes without parking.
type DriveResult struct {
	// ParkedCallID is the id of the long-running call the agent suspended on, or "" when
	// the run finished instead of parking.
	ParkedCallID string
	// ToolResponses maps each tool name to its most recent response this cycle. A tool
	// whose handler returned an error surfaces here with an "error" key (the ADK flow
	// converts a handler error into {"error": message} rather than aborting the run).
	ToolResponses map[string]map[string]any
	// Final is the concatenated text of the agent's non-partial responses.
	Final string
}

// LongRunDriver drives an IsLongRunning agent through ADK's suspend/resume on a single
// in-memory session service. It is the generic plumbing behind a CI-wait loop: all
// domain policy (what to apply, whether to retry, how long to wait) lives in the caller;
// this type only knows how to run-to-park and resume-with-a-result. Keeping it here also
// keeps the genai dependency inside internal/agent/setup (see ARCH).
type LongRunDriver struct {
	r      *runner.Runner
	userID string
}

// NewLongRunDriver builds a driver over root, sharing one in-memory session service so a
// resume lands on the same suspended run a Start parked.
func NewLongRunDriver(appName, userID string, root agent.Agent) (*LongRunDriver, error) {
	r, err := NewRunner(appName, root)
	if err != nil {
		return nil, err
	}
	return &LongRunDriver{r: r, userID: userID}, nil
}

// Start seeds a fresh invocation on sessionID with input and drives until the agent
// parks on a long-running tool or finishes.
func (d *LongRunDriver) Start(ctx context.Context, sessionID, input string) (DriveResult, error) {
	return d.drive(ctx, sessionID, UserText(input))
}

// Resume feeds the real result for a parked long-running call (toolName + callID) back
// into sessionID and drives until the agent re-parks or finishes. It is valid only on a
// session a prior Start/Resume parked; a stale callID resolves to a benign no-op run.
func (d *LongRunDriver) Resume(ctx context.Context, sessionID, callID, toolName string, response map[string]any) (DriveResult, error) {
	content := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: callID, Name: toolName, Response: response},
	}}}
	return d.drive(ctx, sessionID, content)
}

func (d *LongRunDriver) drive(ctx context.Context, sessionID string, input *genai.Content) (DriveResult, error) {
	res := DriveResult{ToolResponses: map[string]map[string]any{}}
	var sb strings.Builder
	for ev, err := range d.r.Run(ctx, d.userID, sessionID, input, agent.RunConfig{}) {
		if err != nil {
			return DriveResult{}, err
		}
		if len(ev.LongRunningToolIDs) > 0 {
			res.ParkedCallID = ev.LongRunningToolIDs[0]
		}
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionResponse != nil {
				res.ToolResponses[p.FunctionResponse.Name] = p.FunctionResponse.Response
			}
		}
		if !ev.Partial {
			sb.WriteString(contentText(ev.Content))
		}
	}
	res.Final = sb.String()
	return res, nil
}

// SequencerConfig configures a deterministic two-phase long-running loop driven by
// NewSequencerModel: call Action (a normal tool), then Wait (a long-running tool that
// suspends the run). When the run resumes with Wait's real result, RetryWhen decides
// whether to loop (call Action again) or conclude.
type SequencerConfig struct {
	Action string // the tool that performs the work and returns a result
	// Wait is the long-running tool that parks the run awaiting an external result. It
	// is called with the Action's result map as its args, so the Wait tool's argument
	// type must accept those fields (extra fields are rejected by strict schema
	// validation).
	Wait string
	// RetryWhen reports whether a resumed Wait result warrants another Action. It may be
	// nil (never retry). Policy that needs out-of-band state (attempt counts, deadlines)
	// belongs in the caller, which simply declines to resume when it does not want a loop.
	RetryWhen func(waitResponse map[string]any) bool
}

// NewSequencerModel returns a model.LLM that emits a fixed Action→Wait tool sequence
// instead of reasoning. It carries no policy: the caller owns retry/stop/timeout and only
// resumes a parked run when it wants another attempt. The substantive LLM work happens
// inside the Action tool's own handler (which may drive real sub-agents).
func NewSequencerModel(cfg SequencerConfig) model.LLM {
	return &sequencer{cfg: cfg}
}

type sequencer struct{ cfg SequencerConfig }

func (s *sequencer) Name() string { return "sequencer:" + s.cfg.Action + "+" + s.cfg.Wait }

func (s *sequencer) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	next := s.decide(req.Contents)
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(next, nil)
	}
}

// decide chooses the next step from the most recent function response in history:
//   - none yet                  -> call Action
//   - Action returned an error  -> conclude (nothing to wait on)
//   - Action returned a result  -> call Wait, forwarding the result as its args
//   - Wait result, RetryWhen    -> call Action again
//   - Wait result, otherwise    -> conclude
func (s *sequencer) decide(contents []*genai.Content) *model.LLMResponse {
	last := lastFunctionResponse(contents)
	switch {
	case last == nil:
		return s.call(s.cfg.Action, nil, contents)
	case last.Name == s.cfg.Action:
		if msg, bad := last.Response["error"]; bad {
			return sequencerText(fmt.Sprintf("%s failed: %v", s.cfg.Action, msg))
		}
		return s.call(s.cfg.Wait, last.Response, contents)
	case last.Name == s.cfg.Wait:
		if s.cfg.RetryWhen != nil && s.cfg.RetryWhen(last.Response) {
			return s.call(s.cfg.Action, nil, contents)
		}
		return sequencerText("done")
	default:
		return sequencerText("done")
	}
}

func (s *sequencer) call(name string, args map[string]any, contents []*genai.Content) *model.LLMResponse {
	if args == nil {
		args = map[string]any{}
	}
	// Unique id per call so the flow correlates each long-running park independently
	// across retries within one session.
	id := fmt.Sprintf("%s_%d", name, countFunctionCalls(contents, name)+1)
	fc := &genai.FunctionCall{ID: id, Name: name, Args: args}
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

func sequencerText(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: AssistantText(text), TurnComplete: true, FinishReason: genai.FinishReasonStop}
}

func lastFunctionResponse(contents []*genai.Content) *genai.FunctionResponse {
	var last *genai.FunctionResponse
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				last = p.FunctionResponse
			}
		}
	}
	return last
}

func countFunctionCalls(contents []*genai.Content, name string) int {
	n := 0
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == name {
				n++
			}
		}
	}
	return n
}
