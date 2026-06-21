package fixflow

import (
	"sync"
	"time"
)

// ParkedRun is one suspended fix run awaiting its CI result. It lives only in memory:
// if the process restarts, parked runs are lost and their PRs are abandoned (an
// accepted trade — see the architecture notes). SessionID + CallID are what a resume
// needs to feed the CI outcome back into the parked run.
type ParkedRun struct {
	SessionID string
	CallID    string
	Attempts  int
	timer     *time.Timer
}

// runRegistry tracks parked runs in memory, keyed by PR. Exactly one of {CI webhook,
// timeout timer} ever resolves a given run: Resolve atomically removes the entry, so
// late or duplicate deliveries (and a timer firing the same instant a webhook lands)
// find nothing and no-op. The registry IS the in-flight record — no DB, no PR scan.
type runRegistry struct {
	mu   sync.Mutex
	runs map[string]*ParkedRun
}

func newRunRegistry() *runRegistry {
	return &runRegistry{runs: make(map[string]*ParkedRun)}
}

// Park records a parked run for prKey and arms its timeout. onTimeout fires once if
// the run is still parked when timeout elapses; it must call Resolve to claim the run
// (and will lose the claim if a webhook got there first).
func (r *runRegistry) Park(prKey string, run *ParkedRun, timeout time.Duration, onTimeout func(prKey string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.runs[prKey]; ok && old.timer != nil {
		old.timer.Stop() // replace any prior parking for this PR (e.g. a retry re-park)
	}
	run.timer = time.AfterFunc(timeout, func() { onTimeout(prKey) })
	r.runs[prKey] = run
}

// Resolve atomically claims and removes the parked run for prKey, stopping its timer.
// Returns (run, true) for the single winner; (nil, false) for late/duplicate callers.
func (r *runRegistry) Resolve(prKey string) (*ParkedRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[prKey]
	if !ok {
		return nil, false
	}
	if run.timer != nil {
		run.timer.Stop()
	}
	delete(r.runs, prKey)
	return run, true
}

// Len reports the number of currently parked runs.
func (r *runRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}
