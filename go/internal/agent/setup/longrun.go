package setup

import (
	"context"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// NodeOutputKey is the reserved key a workflow node sets in its map-typed event Output to
// name itself. Node events carry no engine-level node identity for top-level static nodes,
// so drives attribute outputs to nodes by this self-describing marker instead of relying
// on event order.
const NodeOutputKey = "node"

// DriveResult is the outcome of driving a parking workflow agent through one cycle: until
// it pauses on a request-input park, or finishes without parking.
type DriveResult struct {
	// ParkedCallID is the interrupt id of the request-input park the run paused on, or ""
	// when the run finished instead of parking. Feeding it back to Resume routes the real
	// result to the waiting node.
	ParkedCallID string
	// NodeOutputs are the map-typed event outputs emitted this cycle, in order. Nodes tag
	// their output with NodeOutputKey; NodeOutput selects by that tag.
	NodeOutputs []map[string]any
	// Final is the concatenated text of the run's non-partial responses.
	Final string
}

// NodeOutput returns the most recent output this cycle tagged with the given node name,
// or nil when the node emitted none.
func (r DriveResult) NodeOutput(node string) map[string]any {
	for i := len(r.NodeOutputs) - 1; i >= 0; i-- {
		if r.NodeOutputs[i][NodeOutputKey] == node {
			return r.NodeOutputs[i]
		}
	}
	return nil
}

// LongRunDriver drives a parking workflow agent through the engine's pause/resume on a
// single session service. It is the generic plumbing behind a CI-wait loop: all domain
// policy (what to apply, whether to retry, how long to wait) lives in the caller; this
// type only knows how to run-to-park and resume-with-a-result. Keeping it here also
// keeps the genai dependency inside internal/agent/setup (see ARCH).
//
// Concurrency: a single driver (one runner + one session service) is shared across all
// of an engine's kickoffs and resumes. Each Start/Resume operates on a distinct session
// id, and the session service is keyed by session id, so concurrent drives for different
// runs do not interfere.
type LongRunDriver struct {
	r       *runner.Runner
	sess    session.Service
	appName string
	userID  string
}

// NewLongRunDriver builds a driver over root, using sess as the session service so a
// resume lands on the same paused run a Start parked. A nil sess falls back to an
// in-memory service; a durable sess (sqlite/firestore) makes the parked run survive a
// process restart (its paused state is reconstructed from persisted session events).
func NewLongRunDriver(appName, userID string, root agent.Agent, sess session.Service) (*LongRunDriver, error) {
	if sess == nil {
		sess = session.InMemoryService()
	}
	r, err := newRunner(appName, root, sess)
	if err != nil {
		return nil, err
	}
	return &LongRunDriver{r: r, sess: sess, appName: appName, userID: userID}, nil
}

// DeleteSession removes a session's stored history. Terminal cleanup calls this so a
// durable backend (sqlite/firestore) does not accumulate completed sessions; on the
// in-memory backend it just frees the map entry. Deleting a missing session is a no-op.
func (d *LongRunDriver) DeleteSession(ctx context.Context, sessionID string) error {
	return d.sess.Delete(ctx, &session.DeleteRequest{AppName: d.appName, UserID: d.userID, SessionID: sessionID})
}

// Start seeds a fresh invocation on sessionID with input and drives until the agent
// parks on a request-input pause or finishes.
func (d *LongRunDriver) Start(ctx context.Context, sessionID, input string) (DriveResult, error) {
	return d.drive(ctx, sessionID, UserText(input))
}

// Resume feeds the real result for a parked request-input pause (callID is the interrupt
// id a prior drive parked on) back into sessionID and drives until the agent re-parks or
// finishes. It is valid only on a session a prior Start/Resume parked; the caller is the
// gate against stale resumes — feeding a resolved or unknown callID starts a fresh run
// (which parks again) rather than resuming, so claim the run before resuming it.
func (d *LongRunDriver) Resume(ctx context.Context, sessionID, callID string, response map[string]any) (DriveResult, error) {
	content := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID:       callID,
			Name:     workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": response},
		},
	}}}
	return d.drive(ctx, sessionID, content)
}

func (d *LongRunDriver) drive(ctx context.Context, sessionID string, input *genai.Content) (DriveResult, error) {
	res := DriveResult{}
	var sb strings.Builder
	for ev, err := range d.r.Run(ctx, d.userID, sessionID, input, streamingRunConfig()) {
		if err != nil {
			return DriveResult{}, err
		}
		if len(ev.LongRunningToolIDs) > 0 {
			res.ParkedCallID = ev.LongRunningToolIDs[0]
		}
		if out, ok := ev.Output.(map[string]any); ok {
			res.NodeOutputs = append(res.NodeOutputs, out)
		}
		if ev.Content != nil && !ev.Partial {
			sb.WriteString(contentText(ev.Content))
		}
	}
	res.Final = sb.String()
	return res, nil
}
