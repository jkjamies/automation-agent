package setup

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jkjamies/automation-agent/internal/config"
)

// ParkRecord is one suspended fix run's stored state. It is keyed by SessionID (stable
// from kickoff). Once the run parks awaiting CI it is also indexed by PRKey, which is how
// a CI webhook — which knows only the PR, not our session id — finds the run to resume.
//
// Params is an opaque blob the caller serializes its own run inputs into; the store never
// interprets it, which keeps the store free of caller-specific (fixflow) types and lets
// the same interface back the in-memory, sqlite, and firestore implementations.
type ParkRecord struct {
	SessionID string
	PRKey     string    // empty until the run parks; the resume index
	CallID    string    // the parked long-running call id
	Attempts  int       // attempts made so far (counted by the caller, not GitHub)
	Params    string    // opaque, caller-serialized run inputs
	ParkedAt  time.Time // zero until parked; the sweep cutoff field
}

// Parked reports whether the record is currently parked awaiting CI.
func (r ParkRecord) Parked() bool { return r.PRKey != "" }

// ParkStore persists suspended fix runs so a resume — or, with a durable backend, a
// process restart — can continue them. A record has two distinct lifetimes: the per-run
// record (keyed by SessionID) lives for the whole multi-attempt run, while the PRKey index
// is per-park — claimed by ResolveByPRKey and re-established on each re-park.
//
// Implementations MUST make ResolveByPRKey (and Sweep) an atomic claim: for one PRKey
// exactly one concurrent caller gets ok=true and all others ok=false. That single-winner
// guarantee is what makes a late or duplicate CI webhook — or a timeout racing a webhook —
// safe: the loser finds nothing and no-ops.
type ParkStore interface {
	// Put creates or replaces the per-run record keyed by record.SessionID, (re)establishing
	// the PRKey index when record.PRKey is non-empty.
	Put(ctx context.Context, record ParkRecord) error
	// Get returns the per-run record for sessionID (ok=false if absent).
	Get(ctx context.Context, sessionID string) (ParkRecord, bool, error)
	// ResolveByPRKey atomically claims the parked record for prKey: it clears the PRKey
	// index (so a later duplicate no-ops) and returns the record. The per-run record is
	// retained so a retry can still read its params — terminal cleanup is Delete. ok=false
	// for late/duplicate/unknown callers.
	ResolveByPRKey(ctx context.Context, prKey string) (ParkRecord, bool, error)
	// Delete removes the per-run record (and any lingering index) for sessionID. Terminal
	// cleanup; no-op if absent.
	Delete(ctx context.Context, sessionID string) error
	// Sweep atomically claims and returns every parked record whose ParkedAt is before
	// cutoff (CI never reported). Like ResolveByPRKey, each record is claimed once.
	Sweep(ctx context.Context, cutoff time.Time) ([]ParkRecord, error)
	// ParkedCount reports how many records are currently parked (PRKey-indexed).
	ParkedCount(ctx context.Context) (int, error)
}

// NewParkStore builds the park-record store for the configured backend. Durable
// sqlite/firestore stores land in the follow-up steps; until then every backend uses the
// in-memory store, so parked runs do not yet survive a restart even on SESSION_BACKEND=sqlite.
func NewParkStore(cfg config.Config) (ParkStore, error) {
	switch cfg.SessionBackend {
	case config.SessionMemory, config.SessionSQLite, config.SessionFirestore:
		return NewMemoryParkStore(), nil
	default:
		return nil, fmt.Errorf("unknown session backend %q", cfg.SessionBackend)
	}
}

// memoryParkStore keeps park records in memory: today's behavior, used by tests and by
// any backend until its durable store lands. bySession holds the per-run records; index
// maps an active PRKey to its session id. One mutex guards both, so ResolveByPRKey/Sweep
// claim atomically.
type memoryParkStore struct {
	mu        sync.Mutex
	bySession map[string]ParkRecord
	index     map[string]string // prKey -> sessionID
}

// NewMemoryParkStore returns an in-memory ParkStore.
func NewMemoryParkStore() ParkStore {
	return &memoryParkStore{bySession: map[string]ParkRecord{}, index: map[string]string{}}
}

func (m *memoryParkStore) Put(_ context.Context, r ParkRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Drop a stale index entry if this session was previously parked under a different key.
	if prev, ok := m.bySession[r.SessionID]; ok && prev.PRKey != "" && prev.PRKey != r.PRKey {
		delete(m.index, prev.PRKey)
	}
	m.bySession[r.SessionID] = r
	if r.PRKey != "" {
		m.index[r.PRKey] = r.SessionID
	}
	return nil
}

func (m *memoryParkStore) Get(_ context.Context, sessionID string) (ParkRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.bySession[sessionID]
	return r, ok, nil
}

func (m *memoryParkStore) ResolveByPRKey(_ context.Context, prKey string) (ParkRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sid, ok := m.index[prKey]
	if !ok {
		return ParkRecord{}, false, nil
	}
	return m.claimLocked(prKey, sid), true, nil
}

func (m *memoryParkStore) Sweep(_ context.Context, cutoff time.Time) ([]ParkRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ParkRecord
	for prKey, sid := range m.index {
		if r := m.bySession[sid]; !r.ParkedAt.IsZero() && r.ParkedAt.Before(cutoff) {
			out = append(out, m.claimLocked(prKey, sid))
		}
	}
	return out, nil
}

// claimLocked clears the PRKey index for sid and returns the (now un-parked) record. The
// per-run record is retained for a possible retry. The caller must hold m.mu.
func (m *memoryParkStore) claimLocked(prKey, sid string) ParkRecord {
	delete(m.index, prKey)
	r := m.bySession[sid]
	r.PRKey = ""
	m.bySession[sid] = r
	return r
}

func (m *memoryParkStore) Delete(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.bySession[sessionID]; ok && r.PRKey != "" {
		delete(m.index, r.PRKey)
	}
	delete(m.bySession, sessionID)
	return nil
}

func (m *memoryParkStore) ParkedCount(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.index), nil
}
