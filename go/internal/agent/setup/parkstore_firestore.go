package setup

import (
	"context"
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
	var stale []string
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
			stale = append(stale, d.SessionID)
		}
	}

	out := make([]ParkRecord, 0, len(stale))
	for _, sid := range stale {
		rec, ok, err := s.claimBySession(ctx, sid)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// claimBySession is the sweep's per-doc atomic claim, keyed by session id.
func (s *firestoreParkStore) claimBySession(ctx context.Context, sid string) (ParkRecord, bool, error) {
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
		rec, found, err = claimDoc(tx, snap)
		return err
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
