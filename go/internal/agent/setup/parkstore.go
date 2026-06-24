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
	// cutoff (CI never reported). Like ResolveByPRKey, each record is claimed once, and the
	// returned records keep their PRKey so the caller knows which PR timed out.
	//
	// A claim is re-validated against cutoff inside the atomic step, so a record that was
	// resolved and re-parked (fresh) after the scan is left alone rather than swept.
	// Sweep is best-effort across records: a backend error on one claim does not discard the
	// records already claimed in this pass — they are returned alongside a non-nil error, and
	// the caller MUST process them (notify/clear) before propagating the error, or those
	// claimed runs strand with their PRKey cleared.
	Sweep(ctx context.Context, cutoff time.Time) ([]ParkRecord, error)
	// ParkedCount reports how many records are currently parked (PRKey-indexed).
	ParkedCount(ctx context.Context) (int, error)
}

// NewParkStore builds the park-record store for the configured backend, mirroring the
// session backend. ctx scopes the construction of network-backed stores (firestore).
func NewParkStore(ctx context.Context, cfg config.Config) (ParkStore, error) {
	switch cfg.SessionBackend {
	case config.SessionMemory:
		return NewMemoryParkStore(), nil
	case config.SessionSQLite:
		return NewSQLiteParkStore(cfg.SQLiteDSN)
	case config.SessionFirestore:
		return NewFirestoreParkStore(ctx, cfg.FirestoreProject, cfg.FirestoreCollection+"_parked_runs")
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
	if r.PRKey != "" {
		// One active record per PRKey: if a different session currently owns this key, un-park
		// it so the index has a single winner. Otherwise resolve/sweep could return either
		// session, and a later Delete of the displaced session would strand this one.
		if owner, ok := m.index[r.PRKey]; ok && owner != r.SessionID {
			if prev, ok := m.bySession[owner]; ok {
				prev.PRKey = ""
				m.bySession[owner] = prev
			}
		}
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
	if prKey == "" {
		return ParkRecord{}, false, nil // never resolve by an empty key (parity with sqlite)
	}
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
			claimed := m.claimLocked(prKey, sid)
			claimed.PRKey = prKey // the timeout sweep needs to know which PR this was
			out = append(out, claimed)
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
	// Only drop the index entry while it still points at this session: another session may
	// have since claimed the same PRKey (see Put), and we must not strand its active park.
	if r, ok := m.bySession[sessionID]; ok && r.PRKey != "" {
		if owner, ok := m.index[r.PRKey]; ok && owner == sessionID {
			delete(m.index, r.PRKey)
		}
	}
	delete(m.bySession, sessionID)
	return nil
}

func (m *memoryParkStore) ParkedCount(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.index), nil
}
