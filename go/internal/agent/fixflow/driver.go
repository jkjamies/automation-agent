package fixflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

const (
	toolApplyFix = "apply_fix"
	toolAwaitCI  = "await_ci"
)

// runParams are the per-run inputs the apply_fix tool needs. They are owned by the
// Driver (keyed by session id) and never model-controlled, so a misbehaving model cannot
// redirect which repo or branch is edited.
type runParams struct {
	owner, repo, fullRepo string
	base, report          string
	feedback              string // previous attempt's CI failure, on retry
	newBranch             bool   // true on kickoff (create from base); false on retry (reuse branch)
}

// Driver runs a Spec's CI-wait loop on ADK's IsLongRunning suspend/resume. It owns the
// long-run agent, the in-memory parked-run registry, and each session's run params. All
// policy — retry vs give up, attempt counting, the per-run timeout — lives here; the
// agent's sequencer model only emits a fixed apply_fix→await_ci sequence.
//
// Lifecycle: Kickoff applies a fix and parks on await_ci (registered in the registry).
// A check_run webhook drives Resume, which atomically claims the parked run and either
// notifies success, resumes for another attempt, or gives up at MaxIter. If CI never
// reports, the registry's per-run timer fires onTimeout, which frees the run and asks
// for human review. There is no durable store: a process restart strands parked runs
// (an accepted trade — see the architecture notes).
type Driver struct {
	engine  *Engine
	lr      *setup.LongRunDriver
	reg     *runRegistry
	timeout time.Duration

	mu   sync.Mutex
	runs map[string]*runParams // session id -> params
	seq  uint64                // session id counter (process-local uniqueness)
}

func newDriver(e *Engine) (*Driver, error) {
	dr := &Driver{
		engine:  e,
		reg:     newRunRegistry(),
		timeout: e.d.CITimeout,
		runs:    map[string]*runParams{},
	}
	tools, err := dr.tools()
	if err != nil {
		return nil, err
	}
	seqModel := setup.NewSequencerModel(setup.SequencerConfig{
		Action: toolApplyFix,
		Wait:   toolAwaitCI,
		// The Driver only resumes a run when it has already decided to retry, so a
		// resumed failure always means "apply again". (success/timeout never resume.)
		RetryWhen: func(resp map[string]any) bool { return fmt.Sprint(resp["conclusion"]) == "failure" },
	})
	fixer, err := llmagent.New(llmagent.Config{
		Name:        "fixer-" + e.spec.Name,
		Model:       seqModel,
		Instruction: "Apply the fix, then wait for CI. If CI fails, apply again.",
		Tools:       tools,
	})
	if err != nil {
		return nil, err
	}
	lr, err := setup.NewLongRunDriver("fixflow-"+e.spec.Name, "fixer", fixer)
	if err != nil {
		return nil, err
	}
	dr.lr = lr
	return dr, nil
}

func (dr *Driver) tools() ([]tool.Tool, error) {
	apply, err := functiontool.New(functiontool.Config{
		Name:        toolApplyFix,
		Description: "Apply the fix, commit it, and open or update the PR.",
	}, dr.applyFix)
	if err != nil {
		return nil, err
	}
	await, err := functiontool.New(functiontool.Config{
		Name:          toolAwaitCI,
		Description:   "Wait for CI to report on the PR. Returns a pending status, then the real result later.",
		IsLongRunning: true,
	}, dr.awaitCI)
	if err != nil {
		return nil, err
	}
	return []tool.Tool{apply, await}, nil
}

type applyFixArgs struct{}

type applyFixResult struct {
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha"`
}

// applyFix runs one fix attempt for the calling session. The run params are looked up by
// session id (Driver-owned), so the model's (empty) args cannot influence the target.
func (dr *Driver) applyFix(tc tool.Context, _ applyFixArgs) (applyFixResult, error) {
	rp, ok := dr.runParamsFor(tc.SessionID())
	if !ok {
		return applyFixResult{}, fmt.Errorf("apply_fix: no run params for session %q", tc.SessionID())
	}
	res, err := dr.engine.attemptOnce(tc, rp)
	if err != nil {
		return applyFixResult{}, err
	}
	return applyFixResult{PRNumber: res.PR.Number, HeadSHA: res.HeadSHA}, nil
}

// awaitCIArgs mirrors applyFixResult: the sequencer forwards apply_fix's result as
// await_ci's args, so these fields must stay in sync (strict schema rejects extras).
type awaitCIArgs struct {
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha"`
}

type awaitCIResult struct {
	Status string `json:"status"`
}

// awaitCI is the long-running park point: it records that the run is waiting and returns
// immediately with a pending status. The real CI result is fed back later via Resume.
func (dr *Driver) awaitCI(_ tool.Context, _ awaitCIArgs) (awaitCIResult, error) {
	return awaitCIResult{Status: "pending"}, nil
}

// Kickoff starts a new suspended run: apply the fix, then park awaiting CI.
func (dr *Driver) Kickoff(ctx context.Context, k Kickoff) error {
	sid := dr.newSessionID()
	dr.setRunParams(sid, &runParams{
		owner: k.Owner(), repo: k.Name(), fullRepo: k.Repo,
		base: k.Base, report: k.ReportText(), newBranch: true,
	})
	res, err := dr.lr.Start(ctx, sid, "Apply the fix and wait for CI.")
	if err != nil {
		dr.clear(sid)
		return err
	}
	return dr.afterDrive(ctx, sid, k.Repo, res, 1)
}

// Resume reacts to a CI conclusion for a parked run.
func (dr *Driver) Resume(ctx context.Context, in ResumeInput) error {
	if in.PRNumber == 0 {
		return fmt.Errorf("resume: missing PR number")
	}
	// Only success/failure are actionable. For anything else, leave the run parked so a
	// later conclusive event (or the timeout) resolves it.
	if in.Conclusion != "success" && in.Conclusion != "failure" {
		dr.engine.d.Log.Info("ignoring non-actionable conclusion", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "conclusion", in.Conclusion)
		return nil
	}

	key := prKey(in.FullRepo, in.PRNumber)
	run, ok := dr.reg.Resolve(key)
	if !ok {
		// Late, duplicate, raced with the timeout, or after a restart — nothing to do.
		dr.engine.d.Log.Info("resume: no parked run", "workflow", dr.engine.spec.Name, "pr", key, "conclusion", in.Conclusion)
		return nil
	}
	link := pullURL(in.FullRepo, in.PRNumber)

	if in.Conclusion == "success" {
		dr.clear(run.SessionID)
		dr.engine.d.Log.Info("fix succeeded", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber)
		return dr.engine.notify(ctx, dr.engine.spec.SuccessTitle, fmt.Sprintf("%s: %s passed CI.", in.FullRepo, dr.engine.spec.Name), link)
	}

	// failure
	if run.Attempts >= dr.engine.d.MaxIter {
		dr.clear(run.SessionID)
		dr.engine.d.Log.Warn("fix exhausted attempts", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "attempts", run.Attempts)
		return dr.engine.notify(ctx, dr.engine.spec.ReviewTitle,
			fmt.Sprintf("%s: after %d attempts the %s fix still fails CI. Please review.", in.FullRepo, run.Attempts, dr.engine.spec.Name), link)
	}

	dr.updateForRetry(run.SessionID, in.OutputText)
	res, err := dr.lr.Resume(ctx, run.SessionID, run.CallID, toolAwaitCI, map[string]any{
		"conclusion": in.Conclusion, "output": in.OutputText,
	})
	if err != nil {
		dr.clear(run.SessionID)
		return err
	}
	dr.engine.d.Log.Info("fix retrying", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "attempt", run.Attempts+1)
	return dr.afterDrive(ctx, run.SessionID, in.FullRepo, res, run.Attempts+1)
}

// onTimeout fires (from the registry timer) when a parked run's CI never reports. It
// claims the run, frees it, and asks for human review.
func (dr *Driver) onTimeout(key string) {
	run, ok := dr.reg.Resolve(key)
	if !ok {
		return // already resolved by a webhook
	}
	dr.clear(run.SessionID)
	fullRepo, pr := splitPRKey(key)
	link := pullURL(fullRepo, pr)
	dr.engine.d.Log.Warn("fix timed out waiting for CI", "workflow", dr.engine.spec.Name, "repo", fullRepo, "pr", pr, "timeout", dr.timeout)
	_ = dr.engine.notify(context.Background(), dr.engine.spec.ReviewTitle,
		fmt.Sprintf("%s: the %s fix timed out after %s waiting for CI. Please review.", fullRepo, dr.engine.spec.Name, dr.timeout), link)
}

// afterDrive inspects a drive's outcome and either surfaces an apply error or parks the
// run (and its timeout) under its PR key.
func (dr *Driver) afterDrive(ctx context.Context, sid, fullRepo string, res setup.DriveResult, attempt int) error {
	if apply := res.ToolResponses[toolApplyFix]; apply != nil {
		if msg, bad := apply["error"]; bad {
			return dr.failApply(ctx, sid, fullRepo, fmt.Sprintf("%v", msg))
		}
	}
	if res.ParkedCallID == "" {
		return dr.failApply(ctx, sid, fullRepo, "run did not park on CI wait")
	}
	pr := prNumberFrom(res.ToolResponses[toolApplyFix])
	if pr == 0 {
		return dr.failApply(ctx, sid, fullRepo, "parked without a PR number")
	}
	dr.reg.Park(prKey(fullRepo, pr), &ParkedRun{SessionID: sid, CallID: res.ParkedCallID, Attempts: attempt}, dr.timeout, dr.onTimeout)
	dr.engine.d.Log.Info("fix applied; awaiting CI", "workflow", dr.engine.spec.Name, "repo", fullRepo, "pr", pr, "attempt", attempt)
	return nil
}

// failApply frees a run that errored before it could park on CI (a push/PR/analyze
// failure, not a CI failure) and notifies a human. Without this, an apply error would
// only bubble up to the dispatcher's logger and never reach the review channel — a fix
// that can't even open its PR would vanish silently.
func (dr *Driver) failApply(ctx context.Context, sid, fullRepo, reason string) error {
	dr.clear(sid)
	_ = dr.engine.notify(ctx, dr.engine.spec.ReviewTitle,
		fmt.Sprintf("%s: the %s fix could not be applied (%s). Please review.", fullRepo, dr.engine.spec.Name, reason), "")
	return fmt.Errorf("%s %s: %s", fullRepo, dr.engine.spec.Name, reason)
}

func (dr *Driver) newSessionID() string {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.seq++
	return fmt.Sprintf("run-%d", dr.seq)
}

func (dr *Driver) setRunParams(sid string, rp *runParams) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.runs[sid] = rp
}

func (dr *Driver) runParamsFor(sid string) (*runParams, bool) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	rp, ok := dr.runs[sid]
	return rp, ok
}

func (dr *Driver) updateForRetry(sid, feedback string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if rp, ok := dr.runs[sid]; ok {
		rp.feedback = "The previous attempt failed CI with:\n" + feedback
		rp.newBranch = false
	}
}

func (dr *Driver) clear(sid string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	delete(dr.runs, sid)
}

func prKey(fullRepo string, number int) string { return fmt.Sprintf("%s#%d", fullRepo, number) }

func splitPRKey(key string) (fullRepo string, number int) {
	repo, num, _ := strings.Cut(key, "#")
	n, _ := strconv.Atoi(num)
	return repo, n
}

func prNumberFrom(resp map[string]any) int {
	if resp == nil {
		return 0
	}
	switch v := resp["pr_number"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}
