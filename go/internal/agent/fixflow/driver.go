package fixflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"automation-agent/internal/agent/setup"
	"automation-agent/internal/githubapi"
)

const (
	nodeApplyFix = "apply_fix"
	nodeAwaitCI  = "await_ci"
	nodeConclude = "conclude"

	// routeFixApplied is the one concrete route out of apply_fix: a fix landed on a PR, so
	// proceed to the CI park. Every other apply outcome (clean, error) falls through the
	// Default edge to conclude.
	routeFixApplied = "fix_applied"
	// routeRetry is the one concrete route out of await_ci: CI failed and the caller
	// resumed the run for another attempt, so cycle back to apply_fix. Any other resumed
	// conclusion falls through the Default edge to conclude.
	routeRetry = "failure"
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

// Driver runs a Spec's CI-wait loop on the workflow engine's pause/resume. It owns the
// fixer workflow agent and a ParkStore of parked runs; all policy — retry vs give up,
// attempt counting, the per-run timeout — lives here, while the workflow graph only
// encodes the fixed apply_fix → await_ci shape:
//
//	Start → apply_fix ─"fix_applied"→ await_ci (pause)
//	  apply_fix  ─Default→ conclude   (clean, or the attempt errored)
//	  await_ci   ─"failure"→ apply_fix (the caller resumed for another attempt)
//	  await_ci   ─Default→ conclude   (any other resumed conclusion)
//
// Lifecycle: Kickoff applies a fix and parks on await_ci (recorded in the store). A
// check_run webhook drives Resume, which atomically claims the parked run and either
// notifies success, resumes for another attempt, or gives up at MaxIter. If CI never
// reports, a soft per-run timer fires onTimeout, which frees the run and asks for human
// review. The timer is in-memory (lost on restart); the durable catch-all is the
// ParkStore sweep (wired in a later step). With a durable ParkStore + session backend a
// parked run survives a restart (its paused graph state is rebuilt from the persisted
// session events); with the default in-memory ones it does not.
//
// The ParkStore's atomic claim is the only guard against stale or duplicate CI results:
// a resume fed to a session whose park was already resolved starts a fresh run rather
// than erroring, so Resume must never bypass the claim.
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
	fixer, err := newFixerAgent("fixer-"+e.spec.Name, dr.applyNode, dr.awaitNode)
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

// nodeFunc is the body of an emitting workflow node: it emits its own routing/output
// event and returns nil to suppress the engine's default terminal event.
type nodeFunc = func(agent.Context, any, func(*session.Event) error) (any, error)

// newFixerAgent assembles the fix-loop workflow agent from the apply/await node bodies.
// conclude is the shared terminal node: every non-parked path ends there, so the graph
// always has a terminal and the retry cycle (await_ci → apply_fix) stays conditional.
func newFixerAgent(name string, applyFn, awaitFn nodeFunc) (agent.Agent, error) {
	apply := workflow.NewEmittingFunctionNode[any, any](nodeApplyFix, applyFn, workflow.NodeConfig{})
	rerun := true
	await := workflow.NewEmittingFunctionNode[any, any](nodeAwaitCI, awaitFn, workflow.NodeConfig{
		// Re-entry mode: on resume the node re-runs from scratch and picks the reply up
		// via ResumeOrRequestInput, which is what lets it route on the CI conclusion.
		RerunOnResume: &rerun,
	})
	conclude := workflow.NewFunctionNode[any, string](nodeConclude, func(_ agent.Context, _ any) (string, error) {
		return "done", nil
	}, workflow.NodeConfig{})
	return workflowagent.New(workflowagent.Config{
		Name:        name,
		Description: "Applies a fix, then waits for CI; a resumed failure loops back for another attempt.",
		Edges: []workflow.Edge{
			{From: workflow.Start, To: apply},
			{From: apply, To: await, Route: workflow.StringRoute(routeFixApplied)},
			{From: apply, To: conclude, Route: workflow.Default},
			{From: await, To: apply, Route: workflow.StringRoute(routeRetry)},
			{From: await, To: conclude, Route: workflow.Default},
		},
	})
}

// applyFixOutcome is the wire shape of apply_fix's node output (tagged with
// setup.NodeOutputKey). Clean is true when triage found nothing to address: no PR was
// opened, the run concludes without parking on CI, and afterDrive reports a positive
// "already clean" outcome instead of asking for human review. An attempt that errored
// carries an "error" key instead, so afterDrive can notify a human rather than the
// failure vanishing into a failed run.
type applyFixOutcome struct {
	PRNumber int
	HeadSHA  string
	Clean    bool
	Err      error
}

func (o applyFixOutcome) output() map[string]any {
	out := map[string]any{setup.NodeOutputKey: nodeApplyFix}
	switch {
	case o.Err != nil:
		out["error"] = o.Err.Error()
	case o.Clean:
		out["clean"] = true
	default:
		out["pr_number"] = o.PRNumber
		out["head_sha"] = o.HeadSHA
	}
	return out
}

func (o applyFixOutcome) route() string {
	if o.Err != nil || o.Clean {
		return "conclude" // no matching concrete route: falls through the Default edge
	}
	return routeFixApplied
}

// applyNode runs one fix attempt for the calling session and emits the outcome as the
// node's routing event. The run params are loaded from the store by session id (never
// model- or event-supplied), so nothing in the run's history can redirect which repo or
// branch is edited. An attempt error is reported as an "error" output rather than a node
// failure: the run must conclude (so afterDrive notifies a human), not fail mid-graph.
func (dr *Driver) applyNode(nc agent.Context, _ any, emit func(*session.Event) error) (any, error) {
	outcome := dr.applyFix(nc)
	ev := session.NewEvent(nc, nc.InvocationID())
	ev.Output = outcome.output()
	ev.Routes = []string{outcome.route()}
	if err := emit(ev); err != nil {
		return nil, err
	}
	return nil, nil
}

// applyFix performs the attempt and folds every outcome — including load/decode errors —
// into an applyFixOutcome.
func (dr *Driver) applyFix(nc agent.Context) applyFixOutcome {
	rec, ok, err := dr.store.Get(nc, nc.SessionID())
	if err != nil {
		return applyFixOutcome{Err: fmt.Errorf("apply_fix: load run %q: %w", nc.SessionID(), err)}
	}
	if !ok {
		return applyFixOutcome{Err: fmt.Errorf("apply_fix: no run params for session %q", nc.SessionID())}
	}
	rp, err := unmarshalRunParams(rec.Params)
	if err != nil {
		return applyFixOutcome{Err: fmt.Errorf("apply_fix: decode run %q: %w", nc.SessionID(), err)}
	}
	res, err := dr.engine.attemptOnce(nc, rp)
	if err != nil {
		// Triage finding nothing actionable is not a failure: report it as a clean result
		// so the run concludes and afterDrive sends a positive notice.
		if errors.Is(err, ErrNoWork) {
			return applyFixOutcome{Clean: true}
		}
		return applyFixOutcome{Err: err}
	}
	return applyFixOutcome{PRNumber: res.PR.Number, HeadSHA: res.HeadSHA}
}

// awaitNode is the park point. On its first pass it emits a request-input pause and the
// run suspends until Resume feeds the real CI result back; on the resumed re-entry it
// routes on the conclusion — "failure" cycles back to apply_fix, anything else concludes.
// The interrupt id is derived from the invocation so the re-entered node can correlate
// its own pause; it is unique per run because each fix run owns its session/invocation.
func (dr *Driver) awaitNode(nc agent.Context, _ any, emit func(*session.Event) error) (any, error) {
	reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{
		InterruptID: nodeAwaitCI + "-" + nc.InvocationID(),
		Message:     "Waiting for CI to report on the fix PR.",
	})
	if err != nil {
		return nil, err // first pass: the pause was emitted; the run suspends here
	}
	out := map[string]any{setup.NodeOutputKey: nodeAwaitCI}
	route := "conclude"
	if m, ok := reply.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
		if fmt.Sprint(m["conclusion"]) == "failure" {
			route = routeRetry
		}
	}
	ev := session.NewEvent(nc, nc.InvocationID())
	ev.Output = out
	ev.Routes = []string{route}
	if err := emit(ev); err != nil {
		return nil, err
	}
	return nil, nil
}

// Kickoff starts a new suspended run: apply the fix, then park awaiting CI.
func (dr *Driver) Kickoff(ctx context.Context, k Kickoff) error {
	base, err := dr.resolveBase(ctx, k)
	if err != nil {
		return err
	}
	sid := dr.newSessionID()
	rp := &runParams{
		owner: k.Owner(), repo: k.Name(), fullRepo: k.Repo,
		base: base, report: k.ReportText(), newBranch: true,
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

// resolveBase decides the branch this run works from: the kickoff's explicit base when the
// caller supplied one, otherwise the repository's actual default branch, looked up once here
// and then persisted in the run's params. Resolving once (not per attempt) keeps the branch
// point, the PR base, and the terminal summary's comparison on the same ref for the whole
// multi-attempt run.
//
// A lookup failure fails the kickoff rather than falling back to a guessed name: every
// downstream step (branch point, PR base, compare) needs a ref that really exists, and a
// wrong guess surfaces only as an opaque GitHub 422 when the PR is opened.
func (dr *Driver) resolveBase(ctx context.Context, k Kickoff) (string, error) {
	if k.Base != "" {
		return k.Base, nil
	}
	base, err := dr.engine.d.GH.DefaultBranch(ctx, k.Owner(), k.Name())
	if err != nil {
		return "", fmt.Errorf("%s %s: resolve default branch: %w", k.Repo, dr.engine.spec.Name, err)
	}
	dr.engine.d.Log.Info("resolved base branch", "workflow", dr.engine.spec.Name, "repo", k.Repo, "base", base)
	return base, nil
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
	run, ok, err := dr.store.ResolveByPRKey(ctx, dr.workflow(), key)
	if err != nil {
		return fmt.Errorf("resume: resolve %s: %w", key, err)
	}
	if !ok {
		// Late, duplicate, raced with the timeout, or after a restart — nothing to do.
		dr.engine.d.Log.Info("resume: no parked run", "workflow", dr.engine.spec.Name, "pr", key, "conclusion", in.Conclusion)
		return nil
	}
	dr.stopTimer(key)

	// Notify before clear so the summary is sent while the record is intact, then clear
	// unconditionally (a notify error is returned/logged, not a reason to leak the run). A
	// duplicate webhook cannot double-notify: ResolveByPRKey above already claimed the run.
	if in.Conclusion == "success" {
		dr.engine.d.Log.Info("fix succeeded", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber)
		err := dr.terminalNotify(ctx, outcomeSuccess, dr.engine.spec.SuccessTitle, run, in.FullRepo, in.PRNumber, "")
		dr.clear(ctx, run.SessionID)
		return err
	}

	// failure
	if run.Attempts >= dr.engine.d.MaxIter {
		dr.engine.d.Log.Warn("fix exhausted attempts", "workflow", dr.engine.spec.Name, "repo", in.FullRepo, "pr", in.PRNumber, "attempts", run.Attempts)
		err := dr.terminalNotify(ctx, outcomeExhausted, dr.engine.spec.ReviewTitle, run, in.FullRepo, in.PRNumber, in.OutputText)
		dr.clear(ctx, run.SessionID)
		return err
	}

	if err := dr.updateForRetry(ctx, run.SessionID, in.OutputText); err != nil {
		dr.clear(ctx, run.SessionID)
		return err
	}
	res, err := dr.lr.Resume(ctx, run.SessionID, run.CallID, map[string]any{
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
	run, ok, err := dr.store.ResolveByPRKey(ctx, dr.workflow(), key)
	if err != nil {
		dr.engine.d.Log.Error("timeout resolve failed", "workflow", dr.engine.spec.Name, "pr", key, "err", err)
		return
	}
	if !ok {
		return // already resolved by a webhook
	}
	dr.stopTimer(key)
	fullRepo, pr := splitPRKey(key)
	dr.engine.d.Log.Warn("fix timed out waiting for CI", "workflow", dr.engine.spec.Name, "repo", fullRepo, "pr", pr, "timeout", dr.timeout)
	_ = dr.terminalNotify(ctx, outcomeTimeout, dr.engine.spec.ReviewTitle, run, fullRepo, pr, "")
	dr.clear(ctx, run.SessionID)
}

// SweepTimeouts resolves every parked run whose CI never reported within CITimeout — the
// durable catch-all behind the soft in-memory timer (which a restart loses). Driven by
// Cloud Scheduler via /internal/sweep. The store's Sweep claims each run atomically, so a
// webhook racing the sweep still resolves it at most once.
func (dr *Driver) SweepTimeouts(ctx context.Context) error {
	// Process every record the store claimed even if Sweep also returns an error: the store's
	// contract is that returned records are already claimed (pr_key cleared), so skipping them
	// on error would strand them. Propagate the error afterwards so the handler 500s and Cloud
	// Scheduler retries the records that could not be claimed this pass.
	swept, err := dr.store.Sweep(ctx, dr.workflow(), time.Now().Add(-dr.timeout))
	for _, run := range swept {
		dr.stopTimer(run.PRKey)
		fullRepo, pr := splitPRKey(run.PRKey)
		dr.engine.d.Log.Warn("fix swept after timeout", "workflow", dr.engine.spec.Name, "repo", fullRepo, "pr", pr, "timeout", dr.timeout)
		_ = dr.terminalNotify(ctx, outcomeTimeout, dr.engine.spec.ReviewTitle, run, fullRepo, pr, "")
		dr.clear(ctx, run.SessionID)
	}
	return err
}

// gatherChanges best-effort fetches the PR branch's base...head diff for a terminal
// summary. On error it returns an empty comparison so the summary still reports the attempt
// count and findings.
func (dr *Driver) gatherChanges(ctx context.Context, rp *runParams) githubapi.Comparison {
	cmp, err := dr.engine.d.GH.Compare(ctx, rp.owner, rp.repo, rp.base, dr.engine.spec.Branch)
	if err != nil {
		dr.engine.d.Log.Warn("compare for summary failed", "workflow", dr.engine.spec.Name, "repo", rp.fullRepo, "err", err)
		return githubapi.Comparison{}
	}
	return cmp
}

// terminalNotify builds and sends the status-aware summary for a finished run: the outcome
// framing, the original targeted findings, and what actually changed on the PR.
func (dr *Driver) terminalNotify(ctx context.Context, outcome terminalOutcome, title string, run setup.ParkRecord, fullRepo string, prNumber int, lastOutput string) error {
	in := summaryInput{
		outcome: outcome, workflow: dr.engine.spec.Name, fullRepo: fullRepo,
		prNumber: prNumber, attempts: run.Attempts, lastOutput: lastOutput,
		timeout: dr.timeout.String(), checkName: dr.engine.spec.CheckName,
	}
	if rp, err := unmarshalRunParams(run.Params); err == nil {
		in.report = rp.report
		in.changed = dr.gatherChanges(ctx, rp)
	} else {
		dr.engine.d.Log.Warn("decode run params for summary failed; sending without findings/diff",
			"workflow", dr.engine.spec.Name, "session", run.SessionID, "err", err)
	}
	return dr.engine.notify(ctx, title, buildSummaryText(in), pullURL(fullRepo, prNumber))
}

// afterDrive inspects a drive's outcome and either surfaces an apply error or parks the
// run (and arms its timeout) under its PR key.
func (dr *Driver) afterDrive(ctx context.Context, sid, fullRepo string, res setup.DriveResult, attempt int) error {
	apply := res.NodeOutput(nodeApplyFix)
	if apply != nil {
		if msg, bad := apply["error"]; bad {
			return dr.failApply(ctx, sid, fullRepo, fmt.Sprintf("%v", msg))
		}
		if clean, _ := apply["clean"].(bool); clean {
			return dr.finishClean(ctx, sid, fullRepo)
		}
	}
	if res.ParkedCallID == "" {
		return dr.failApply(ctx, sid, fullRepo, "run did not park on CI wait")
	}
	pr := prNumberFrom(apply)
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

// finishClean resolves a run whose triage found nothing to address. No PR was opened and
// the run never parked, so it just frees the run and sends a positive "already clean"
// notice — never the human-review alarm. Returns nil so the dispatcher does not log a
// no-op as a failure.
func (dr *Driver) finishClean(ctx context.Context, sid, fullRepo string) error {
	dr.engine.d.Log.Info("nothing to address; already clean", "workflow", dr.engine.spec.Name, "repo", fullRepo)
	dr.clear(ctx, sid)
	text := buildSummaryText(summaryInput{outcome: outcomeClean, workflow: dr.engine.spec.Name, fullRepo: fullRepo})
	return dr.engine.notify(ctx, dr.engine.spec.CleanTitle, text, "")
}

// newSessionID returns a globally unique session id. A UUID (not a process-local counter)
// is required because the ParkStore is shared across Drivers and, with a durable backend,
// across restarts and instances — a counter would collide or overwrite persisted runs.
func (dr *Driver) newSessionID() string {
	return uuid.NewString()
}

// workflow is this driver's owning engine name ("lint" | "coverage"). Every engine shares
// one ParkStore, so it stamps each record and scopes every claim — see setup.ParkRecord.
func (dr *Driver) workflow() string { return dr.engine.spec.Name }

// putParams stores a fresh run's inputs (not yet parked: no PR key, no timer).
func (dr *Driver) putParams(ctx context.Context, sid string, rp *runParams) error {
	blob, err := marshalRunParams(rp)
	if err != nil {
		return err
	}
	return dr.store.Put(ctx, setup.ParkRecord{SessionID: sid, Workflow: dr.workflow(), Params: blob})
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
	rec.Workflow = dr.workflow() // stamp on every write: the claim scope must never be blank
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

// clear is terminal cleanup: it removes the run from the park store and deletes the ADK
// session so a durable backend does not leak completed runs. (The timer, if any, is
// stopped by the resolve that precedes clear.)
func (dr *Driver) clear(ctx context.Context, sid string) {
	if err := dr.store.Delete(ctx, sid); err != nil {
		dr.engine.d.Log.Error("clear run failed", "workflow", dr.engine.spec.Name, "session", sid, "err", err)
	}
	if err := dr.lr.DeleteSession(ctx, sid); err != nil {
		dr.engine.d.Log.Error("delete session failed", "workflow", dr.engine.spec.Name, "session", sid, "err", err)
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

// parkedCount reports the number of this engine's currently parked runs (used by tests).
func (dr *Driver) parkedCount() int {
	n, _ := dr.store.ParkedCount(context.Background(), dr.workflow())
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
