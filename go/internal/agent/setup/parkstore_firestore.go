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
// key cannot leak a stale entry.
type firestoreParkDoc struct {
	SessionID string    `firestore:"session_id"`
	PRKey     string    `firestore:"pr_key"`
	CallID    string    `firestore:"call_id"`
	Attempts  int       `firestore:"attempts"`
	Params    string    `firestore:"params"`
	ParkedAt  time.Time `firestore:"parked_at"`
}

func (d firestoreParkDoc) toRecord() ParkRecord {
	return ParkRecord{
		SessionID: d.SessionID, PRKey: d.PRKey, CallID: d.CallID,
		Attempts: d.Attempts, Params: d.Params, ParkedAt: d.ParkedAt,
	}
}

func parkDocFromRecord(r ParkRecord) firestoreParkDoc {
	return firestoreParkDoc{
		SessionID: r.SessionID, PRKey: r.PRKey, CallID: r.CallID,
		Attempts: r.Attempts, Params: r.Params, ParkedAt: r.ParkedAt,
	}
}

// firestoreParkStore persists park records to Firestore — the serverless, scale-to-zero
// cloud backend. The atomic claim runs in a Firestore transaction: of N concurrent
// resolvers, the first to commit clears pr_key; the others' transactions detect the change
// and retry, re-read the now-cleared key, and find nothing — so exactly one wins.
type firestoreParkStore struct {
	client *firestore.Client
	coll   string
}

// NewFirestoreParkStore opens a Firestore-backed park store. project may be "" to detect it
// from ADC / GOOGLE_CLOUD_PROJECT. Close releases the client.
func NewFirestoreParkStore(ctx context.Context, project, collection string) (*firestoreParkStore, error) {
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
		// One active doc per pr_key: clear it on any OTHER session still holding it, so
		// resolve/sweep have a single winner. Best-effort (not transactional with the Set).
		docs, err := s.col().Where("pr_key", "==", r.PRKey).Documents(ctx).GetAll()
		if err != nil {
			return err
		}
		for _, snap := range docs {
			if snap.Ref.ID == r.SessionID {
				continue
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

func (s *firestoreParkStore) ResolveByPRKey(ctx context.Context, prKey string) (ParkRecord, bool, error) {
	if prKey == "" {
		return ParkRecord{}, false, nil // an empty key would match unparked docs (pr_key="")
	}
	var rec ParkRecord
	var found bool
	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		found = false // reset on each retry
		docs, err := tx.Documents(s.col().Where("pr_key", "==", prKey).Limit(1)).GetAll()
		if err != nil {
			return err
		}
		if len(docs) == 0 {
			return nil
		}
		rec, found, err = claimDoc(tx, docs[0])
		return err
	})
	if err != nil {
		return ParkRecord{}, false, err
	}
	return rec, found, nil
}

func (s *firestoreParkStore) Sweep(ctx context.Context, cutoff time.Time) ([]ParkRecord, error) {
	// Collect candidate session ids (parked + stale), then claim each in its own
	// transaction so a concurrent resolve cannot double-claim.
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
		if !d.ParkedAt.IsZero() && d.ParkedAt.Before(cutoff) {
			candidates = append(candidates, stale{d.SessionID, d.PRKey})
		}
	}

	out := make([]ParkRecord, 0, len(candidates))
	var errs []error
	for _, c := range candidates {
		// Claim each candidate; a per-doc error skips it (it stays parked for the next sweep)
		// rather than discarding the docs already claimed this pass.
		rec, ok, err := s.claimStaleBySession(ctx, c.sessionID, c.prKey, cutoff)
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
// transaction it re-checks that the doc still carries the expected (stale) pr_key and is
// still older than cutoff, so a concurrent resolve+re-park between the scan and the claim
// leaves the fresh park untouched instead of clearing it with a false timeout.
func (s *firestoreParkStore) claimStaleBySession(ctx context.Context, sid, prKey string, cutoff time.Time) (ParkRecord, bool, error) {
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
		if d.PRKey != prKey || d.ParkedAt.IsZero() || !d.ParkedAt.Before(cutoff) {
			return nil // resolved and/or re-parked since the scan — not ours to sweep
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

func (s *firestoreParkStore) ParkedCount(ctx context.Context) (int, error) {
	it := s.col().Where("pr_key", "!=", "").Documents(ctx)
	defer it.Stop()
	n := 0
	for {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}
