package fixflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jkjamies/automation-agent/internal/agent/setup"
)

const (
	toolApplyFix = "apply_fix"
	toolAwaitCI  = "await_ci"
)

// runParams are the per-run inputs the apply_fix tool needs. They are looked up by session
// id and never model-controlled, so a misbehaving model cannot redirect which repo or
// branch is edited. They are persisted (serialized) in the ParkStore so a retry — or, with
// a durable backend, a restart — can reconstruct them.
type runParams struct {
	owner, repo, fullRepo string
	base, report          string
	feedback              string // previous attempt's CI failure, on retry
	newBranch             bool   // true on kickoff (create from base); false on retry (reuse branch)
}

// runParamsJSON is the serialized form stored in ParkRecord.Params. runParams' own fields
// are unexported (so only this package can build them), so an explicit shim does the
// marshalling rather than reflecting over the struct directly.
type runParamsJSON struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	FullRepo  string `json:"full_repo"`
	Base      string `json:"base"`
	Report    string `json:"report"`
	Feedback  string `json:"feedback"`
	NewBranch bool   `json:"new_branch"`
}

func marshalRunParams(rp *runParams) (string, error) {
	b, err := json.Marshal(runParamsJSON{
		Owner: rp.owner, Repo: rp.repo, FullRepo: rp.fullRepo,
		Base: rp.base, Report: rp.report, Feedback: rp.feedback, NewBranch: rp.newBranch,
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalRunParams(s string) (*runParams, error) {
	var j runParamsJSON
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return nil, err
	}
	return &runParams{
		owner: j.Owner, repo: j.Repo, fullRepo: j.FullRepo,
		base: j.Base, report: j.Report, feedback: j.Feedback, newBranch: j.NewBranch,
	}, nil
}

// Driver runs a Spec's CI-wait loop on ADK's IsLongRunning suspend/resume. It owns the
// long-run agent and a ParkStore of suspended runs; all policy — retry vs give up, attempt
// counting, the per-run timeout — lives here, while the agent's sequencer model only emits
// a fixed apply_fix→await_ci sequence.
//
// Lifecycle: Kickoff applies a fix and parks on await_ci (recorded in the store). A
// check_run webhook drives Resume, which atomically claims the parked run and either
// notifies success, resumes for another attempt, or gives up at MaxIter. If CI never
// reports, a soft per-run timer fires onTimeout, which frees the run and asks for human
// review. The timer is in-memory (lost on restart); the durable catch-all is the
// ParkStore sweep (wired in a later step). With a durable ParkStore + session backend a
// parked run survives a restart; with the default in-memory ones it does not.
type Driver struct {
	engine  *Engine
	lr      *setup.LongRunDriver
	store   setup.ParkStore
	timeout time.Duration

	mu     sync.Mutex
	timers map[string]*time.Timer // prKey -> soft timeout timer
}

func newDriver(e *Engine) (*Driver, error) {
	store := e.d.ParkStore
	if store == nil {
		store = setup.NewMemoryParkStore()
	}
	dr := &Driver{
		engine:  e,
		store:   store,
		timeout: e.d.CITimeout,
		timers:  map[string]*time.Timer{},
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
	lr, err := setup.NewLongRunDriver("fixflow-"+e.spec.Name, "fixer", fixer, e.d.SessionService)
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

// applyFix runs one fix attempt for the calling session. The run params are loaded from the
// store by session id (never model-supplied), so the model's (empty) args cannot influence
// the target.
func (dr *Driver) applyFix(tc tool.Context, _ applyFixArgs) (applyFixResult, error) {
	rec, ok, err := dr.store.Get(tc, tc.SessionID())
	if err != nil {
		return applyFixResult{}, fmt.Errorf("apply_fix: load run %q: %w", tc.SessionID(), err)
	}
	if !ok {
		return applyFixResult{}, fmt.Errorf("apply_fix: no run params for session %q", tc.SessionID())
	}
	rp, err := unmarshalRunParams(rec.Params)
	if err != nil {
		return applyFixResult{}, fmt.Errorf("apply_fix: decode run %q: %w", tc.SessionID(), err)
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
	rp := &runParams{
		owner: k.Owner(), repo: k.Name(), fullRepo: k.Repo,
		base: k.Base, report: k.ReportText(), newBranch: true,
	}
	if err := dr.putParams(ctx, sid, rp); err != nil {
		return err
	}
	res, err := dr.lr.Start(ctx, sid, "Apply the fix and wait for CI.")
	if err != nil {
		dr.clear(ctx, sid)
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
	run, ok, err := dr.store.ResolveByPRKey(ctx, key)
	if err != nil {
		return fmt.Errorf("resume: resolve %s: %w", key, err)
	}
	if !ok {
		// Late, duplicate, raced with the timeout, or after a restart — nothing to do.
		dr.engine.d.Log.Info("resume: no parked run", "workflow", dr.engine.spec.Name, "pr", key, "conclusion", in.Conclusion)
		return nil
	}
	dr.stopTimer(key)
	link := pullURL(in.FullRepo, in.PRNumber)

	if in.Conclusion == "success" {
		dr.clear(ctx, run.SessionID)
		dr.engine.d.Log.Info("fix succeeded", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber)
		return dr.engine.notify(ctx, dr.engine.spec.SuccessTitle, fmt.Sprintf("%s: %s passed CI.", in.FullRepo, dr.engine.spec.Name), link)
	}

	// failure
	if run.Attempts >= dr.engine.d.MaxIter {
		dr.clear(ctx, run.SessionID)
		dr.engine.d.Log.Warn("fix exhausted attempts", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "attempts", run.Attempts)
		return dr.engine.notify(ctx, dr.engine.spec.ReviewTitle,
			fmt.Sprintf("%s: after %d attempts the %s fix still fails CI. Please review.", in.FullRepo, run.Attempts, dr.engine.spec.Name), link)
	}

	if err := dr.updateForRetry(ctx, run.SessionID, in.OutputText); err != nil {
		dr.clear(ctx, run.SessionID)
		return err
	}
	res, err := dr.lr.Resume(ctx, run.SessionID, run.CallID, toolAwaitCI, map[string]any{
		"conclusion": in.Conclusion, "output": in.OutputText,
	})
	if err != nil {
		dr.clear(ctx, run.SessionID)
		return err
	}
	dr.engine.d.Log.Info("fix retrying", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "attempt", run.Attempts+1)
	return dr.afterDrive(ctx, run.SessionID, in.FullRepo, res, run.Attempts+1)
}

// onTimeout fires (from the soft per-run timer) when a parked run's CI never reports. It
// claims the run, frees it, and asks for human review.
func (dr *Driver) onTimeout(key string) {
	ctx := context.Background()
	run, ok, err := dr.store.ResolveByPRKey(ctx, key)
	if err != nil {
		dr.engine.d.Log.Error("timeout resolve failed", "workflow", dr.engine.spec.Name, "pr", key, "err", err)
		return
	}
	if !ok {
		return // already resolved by a webhook
	}
	dr.stopTimer(key)
	dr.clear(ctx, run.SessionID)
	fullRepo, pr := splitPRKey(key)
	link := pullURL(fullRepo, pr)
	dr.engine.d.Log.Warn("fix timed out waiting for CI", "workflow", dr.engine.spec.Name, "repo", fullRepo, "pr", pr, "timeout", dr.timeout)
	_ = dr.engine.notify(ctx, dr.engine.spec.ReviewTitle,
		fmt.Sprintf("%s: the %s fix timed out after %s waiting for CI. Please review.", fullRepo, dr.engine.spec.Name, dr.timeout), link)
}

// afterDrive inspects a drive's outcome and either surfaces an apply error or parks the
// run (and arms its timeout) under its PR key.
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
	key := prKey(fullRepo, pr)
	if err := dr.park(ctx, sid, key, res.ParkedCallID, attempt); err != nil {
		return dr.failApply(ctx, sid, fullRepo, fmt.Sprintf("could not record parked run: %v", err))
	}
	dr.engine.d.Log.Info("fix applied; awaiting CI", "workflow", dr.engine.spec.Name, "repo", fullRepo, "pr", pr, "attempt", attempt)
	return nil
}

// failApply frees a run that errored before it could park on CI (a push/PR/analyze
// failure, not a CI failure) and notifies a human. Without this, an apply error would
// only bubble up to the dispatcher's logger and never reach the review channel — a fix
// that can't even open its PR would vanish silently.
func (dr *Driver) failApply(ctx context.Context, sid, fullRepo, reason string) error {
	dr.clear(ctx, sid)
	_ = dr.engine.notify(ctx, dr.engine.spec.ReviewTitle,
		fmt.Sprintf("%s: the %s fix could not be applied (%s). Please review.", fullRepo, dr.engine.spec.Name, reason), "")
	return fmt.Errorf("%s %s: %s", fullRepo, dr.engine.spec.Name, reason)
}

// newSessionID returns a globally unique session id. A UUID (not a process-local counter)
// is required because the ParkStore is shared across Drivers and, with a durable backend,
// across restarts and instances — a counter would collide or overwrite persisted runs.
func (dr *Driver) newSessionID() string {
	return uuid.NewString()
}

// putParams stores a fresh run's inputs (not yet parked: no PR key, no timer).
func (dr *Driver) putParams(ctx context.Context, sid string, rp *runParams) error {
	blob, err := marshalRunParams(rp)
	if err != nil {
		return err
	}
	return dr.store.Put(ctx, setup.ParkRecord{SessionID: sid, Params: blob})
}

// park records that sid is now suspended awaiting CI under key, and arms the soft timeout.
// It preserves the run's stored params (read-modify-write of the existing record).
func (dr *Driver) park(ctx context.Context, sid, key, callID string, attempt int) error {
	rec, ok, err := dr.store.Get(ctx, sid)
	if err != nil {
		return err
	}
	if !ok {
		rec = setup.ParkRecord{SessionID: sid}
	}
	rec.PRKey = key
	rec.CallID = callID
	rec.Attempts = attempt
	rec.ParkedAt = time.Now()
	if err := dr.store.Put(ctx, rec); err != nil {
		return err
	}
	dr.armTimer(key)
	return nil
}

// updateForRetry records the previous attempt's CI failure as feedback and switches the
// run off branch-creation, persisting the change for the retry's apply_fix.
func (dr *Driver) updateForRetry(ctx context.Context, sid, feedback string) error {
	rec, ok, err := dr.store.Get(ctx, sid)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	rp, err := unmarshalRunParams(rec.Params)
	if err != nil {
		return err
	}
	rp.feedback = "The previous attempt failed CI with:\n" + feedback
	rp.newBranch = false
	blob, err := marshalRunParams(rp)
	if err != nil {
		return err
	}
	rec.Params = blob
	return dr.store.Put(ctx, rec)
}

// clear is terminal cleanup: it removes the run from the store and stops any timer.
func (dr *Driver) clear(ctx context.Context, sid string) {
	if err := dr.store.Delete(ctx, sid); err != nil {
		dr.engine.d.Log.Error("clear run failed", "workflow", dr.engine.spec.Name, "session", sid, "err", err)
	}
}

func (dr *Driver) armTimer(key string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if old, ok := dr.timers[key]; ok {
		old.Stop() // replace any prior parking for this PR (e.g. a retry re-park)
	}
	dr.timers[key] = time.AfterFunc(dr.timeout, func() { dr.onTimeout(key) })
}

func (dr *Driver) stopTimer(key string) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if t, ok := dr.timers[key]; ok {
		t.Stop()
		delete(dr.timers, key)
	}
}

// parkedCount reports the number of currently parked runs (used by tests).
func (dr *Driver) parkedCount() int {
	n, _ := dr.store.ParkedCount(context.Background())
	return n
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
