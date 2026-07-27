package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// firestoreParkDoc is the Firestore document backing a park record. As with sqlite, the
// pr_key field doubles as the resume index ("" when not parked), so re-parking under a new
// key cannot leak a stale entry, and workflow scopes every claim to the owning engine.
type firestoreParkDoc struct {
	SessionID string    `firestore:"session_id"`
	Workflow  string    `firestore:"workflow"`
	PRKey     string    `firestore:"pr_key"`
	CallID    string    `firestore:"call_id"`
	Attempts  int       `firestore:"attempts"`
	Params    string    `firestore:"params"`
	UpdatedAt time.Time `firestore:"updated_at"`
}

// firestoreParkDoc mirrors ParkRecord field for field (only the firestore tags differ), so
// these are plain conversions — a divergence becomes a compile error rather than a field
// silently dropped on the way to or from storage.
func (d firestoreParkDoc) toRecord() ParkRecord { return ParkRecord(d) }

func parkDocFromRecord(r ParkRecord) firestoreParkDoc { return firestoreParkDoc(r) }

// firestoreParkStore persists park records to Firestore — the serverless, scale-to-zero
// cloud backend. Every fix engine shares this one instance and collection; the workflow
// field is what keeps their claims disjoint. The atomic claim runs in a Firestore
// transaction: of N concurrent resolvers, the first to commit clears pr_key; the others'
// transactions detect the change and retry, re-read the now-cleared key, and find nothing —
// so exactly one wins.
//
// Every query here filters on a SINGLE field (pr_key) and narrows by workflow in Go, so the
// store needs no composite index — matching the deployment promise that a Native-mode
// database works with nothing to pre-create. The candidate sets are tiny (at most one doc
// per workflow per PR), so the client-side narrowing costs nothing.
type firestoreParkStore struct {
	client *firestore.Client
	coll   string
}

// NewFirestoreParkStore opens a Firestore-backed park store. project may be "" to detect it
// from ADC / GOOGLE_CLOUD_PROJECT. The returned store also implements io.Closer, which is how
// the entrypoint releases the client at shutdown (it type-asserts rather than depending on
// the concrete type).
func NewFirestoreParkStore(ctx context.Context, project, collection string) (ParkStore, error) {
	if project == "" {
		project = firestore.DetectProjectID
	}
	client, err := firestore.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("firestore client: %w", err)
	}
	return &firestoreParkStore{client: client, coll: collection}, nil
}

// Close releases the underlying Firestore client.
func (s *firestoreParkStore) Close() error { return s.client.Close() }

func (s *firestoreParkStore) col() *firestore.CollectionRef { return s.client.Collection(s.coll) }

func (s *firestoreParkStore) Put(ctx context.Context, r ParkRecord) error {
	if r.PRKey != "" {
		// One active doc per (workflow, pr_key): clear it on any OTHER session of the SAME
		// workflow still holding it, so resolve/sweep have a single winner. A sibling engine
		// parked on the same PR number is left alone. Best-effort (not transactional with the Set).
		docs, err := s.col().Where("pr_key", "==", r.PRKey).Documents(ctx).GetAll()
		if err != nil {
			return err
		}
		for _, snap := range docs {
			if snap.Ref.ID == r.SessionID {
				continue
			}
			var d firestoreParkDoc
			if err := snap.DataTo(&d); err != nil {
				return err
			}
			if d.Workflow != r.Workflow {
				continue // another engine's park on the same PR key
			}
			if _, err := snap.Ref.Update(ctx, []firestore.Update{{Path: "pr_key", Value: ""}}); err != nil {
				return err
			}
		}
	}
	_, err := s.col().Doc(r.SessionID).Set(ctx, parkDocFromRecord(r))
	return err
}

func (s *firestoreParkStore) Get(ctx context.Context, sessionID string) (ParkRecord, bool, error) {
	snap, err := s.col().Doc(sessionID).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return ParkRecord{}, false, nil
	}
	if err != nil {
		return ParkRecord{}, false, err
	}
	var d firestoreParkDoc
	if err := snap.DataTo(&d); err != nil {
		return ParkRecord{}, false, err
	}
	return d.toRecord(), true, nil
}

func (s *firestoreParkStore) ResolveByPRKey(ctx context.Context, workflow, prKey string) (ParkRecord, bool, error) {
	if prKey == "" {
		return ParkRecord{}, false, nil // an empty key would match unparked docs (pr_key="")
	}
	var rec ParkRecord
	var found bool
	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		found = false // reset on each retry
		// Query the single indexed field, then pick this workflow's doc in Go (no composite
		// index). At most one doc per workflow holds a given pr_key, so this selects uniquely.
		docs, err := tx.Documents(s.col().Where("pr_key", "==", prKey)).GetAll()
		if err != nil {
			return err
		}
		for _, snap := range docs {
			var d firestoreParkDoc
			if err := snap.DataTo(&d); err != nil {
				return err
			}
			if d.Workflow != workflow {
				continue // another engine's run: not ours to claim
			}
			rec, found, err = claimDoc(tx, snap)
			return err
		}
		return nil
	})
	if err != nil {
		return ParkRecord{}, false, err
	}
	return rec, found, nil
}

func (s *firestoreParkStore) Sweep(ctx context.Context, workflow string, cutoff time.Time) ([]ParkRecord, error) {
	// Collect candidate session ids (this workflow's, parked + stale), then claim each in its
	// own transaction so a concurrent resolve cannot double-claim.
	it := s.col().Where("pr_key", "!=", "").Documents(ctx)
	defer it.Stop()
	type stale struct{ sessionID, prKey string }
	var candidates []stale
	for {
		snap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var d firestoreParkDoc
		if err := snap.DataTo(&d); err != nil {
			return nil, err
		}
		if d.Workflow != workflow {
			continue // another engine's run: not ours to sweep
		}
		if !d.UpdatedAt.IsZero() && d.UpdatedAt.Before(cutoff) {
			candidates = append(candidates, stale{d.SessionID, d.PRKey})
		}
	}

	out := make([]ParkRecord, 0, len(candidates))
	var errs []error
	for _, c := range candidates {
		// Claim each candidate; a per-doc error skips it (it stays parked for the next sweep)
		// rather than discarding the docs already claimed this pass.
		rec, ok, err := s.claimStaleBySession(ctx, workflow, c.sessionID, c.prKey, cutoff)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			rec.PRKey = c.prKey // restore for the caller (timeout sweep needs the PR)
			out = append(out, rec)
		}
	}
	return out, errors.Join(errs...)
}

// claimStaleBySession is the sweep's per-doc atomic claim, keyed by session id. Inside the
// transaction it re-checks that the doc still belongs to this workflow, still carries the
// expected (stale) pr_key, and is still older than cutoff, so a concurrent resolve+re-park
// between the scan and the claim leaves the fresh park untouched instead of clearing it
// with a false timeout.
func (s *firestoreParkStore) claimStaleBySession(ctx context.Context, workflow, sid, prKey string, cutoff time.Time) (ParkRecord, bool, error) {
	var rec ParkRecord
	var found bool
	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		found = false
		snap, err := tx.Get(s.col().Doc(sid))
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if err != nil {
			return err
		}
		var d firestoreParkDoc
		if err := snap.DataTo(&d); err != nil {
			return err
		}
		if d.Workflow != workflow || d.PRKey != prKey || d.UpdatedAt.IsZero() || !d.UpdatedAt.Before(cutoff) {
			return nil // another workflow's, or resolved and/or re-parked since the scan
		}
		if err := tx.Update(snap.Ref, []firestore.Update{{Path: "pr_key", Value: ""}}); err != nil {
			return err
		}
		d.PRKey = ""
		rec, found = d.toRecord(), true
		return nil
	})
	if err != nil {
		return ParkRecord{}, false, err
	}
	return rec, found, nil
}

// claimDoc clears a still-parked doc's pr_key inside a transaction and returns the claimed
// record. A doc already cleared (pr_key=="") yields found=false so a racing claimer no-ops.
// The caller must perform all transaction reads before invoking this (it writes).
func claimDoc(tx *firestore.Transaction, snap *firestore.DocumentSnapshot) (ParkRecord, bool, error) {
	var d firestoreParkDoc
	if err := snap.DataTo(&d); err != nil {
		return ParkRecord{}, false, err
	}
	if d.PRKey == "" {
		return ParkRecord{}, false, nil
	}
	if err := tx.Update(snap.Ref, []firestore.Update{{Path: "pr_key", Value: ""}}); err != nil {
		return ParkRecord{}, false, err
	}
	d.PRKey = ""
	return d.toRecord(), true, nil
}

func (s *firestoreParkStore) Delete(ctx context.Context, sessionID string) error {
	_, err := s.col().Doc(sessionID).Delete(ctx)
	return err
}

// SweepOrphans queries the single indexed field (pr_key == "") and narrows by workflow and
// age in Go, so no composite index is needed — the same approach the other queries here take.
func (s *firestoreParkStore) SweepOrphans(ctx context.Context, workflow string, cutoff time.Time) ([]ParkRecord, error) {
	it := s.col().Where("pr_key", "==", "").Documents(ctx)
	defer it.Stop()
	var out []ParkRecord
	for {
		snap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var d firestoreParkDoc
		if err := snap.DataTo(&d); err != nil {
			return nil, err
		}
		if d.Workflow == workflow && !d.UpdatedAt.IsZero() && d.UpdatedAt.Before(cutoff) {
			out = append(out, d.toRecord())
		}
	}
	return out, nil
}

func (s *firestoreParkStore) ParkedCount(ctx context.Context, workflow string) (int, error) {
	it := s.col().Where("pr_key", "!=", "").Documents(ctx)
	defer it.Stop()
	n := 0
	for {
		snap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return 0, err
		}
		var d firestoreParkDoc
		if err := snap.DataTo(&d); err != nil {
			return 0, err
		}
		if d.Workflow == workflow {
			n++
		}
	}
	return n, nil
}
