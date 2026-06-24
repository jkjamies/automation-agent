package setup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
	"google.golang.org/adk/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// firestoreSessionService is a custom session.Service backed by Firestore — the cloud
// durable session store (adk-go ships only inmemory/database/vertexai). It mirrors the
// in-memory service's semantics: app:/user:/temp: state scopes, partial-event skipping,
// and event filtering on read. State scope routing replicates internal/sessionutils
// (which is not importable). Events are stored as JSON because their genai content does
// not map cleanly onto Firestore's value types.
type firestoreSessionService struct {
	client          *firestore.Client
	sessions, appSt string
	userSt          string
}

// NewFirestoreSessionService opens a Firestore-backed session service. project may be ""
// to detect it from ADC / GOOGLE_CLOUD_PROJECT. Close releases the client.
func NewFirestoreSessionService(ctx context.Context, project, prefix string) (*firestoreSessionService, error) {
	if project == "" {
		project = firestore.DetectProjectID
	}
	client, err := firestore.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("firestore client: %w", err)
	}
	return &firestoreSessionService{
		client:   client,
		sessions: prefix + "_sessions",
		appSt:    prefix + "_app_state",
		userSt:   prefix + "_user_state",
	}, nil
}

// Close releases the underlying Firestore client.
func (s *firestoreSessionService) Close() error { return s.client.Close() }

var _ session.Service = (*firestoreSessionService)(nil)

// --- Firestore document shapes ---

// sessionDoc is the persisted session: session-scoped state (app/user state live in their
// own collections). Events live in an "events" sub-collection (see eventDoc) rather than an
// array field, so a long-lived session cannot blow Firestore's 1 MiB per-document limit.
type sessionDoc struct {
	AppName   string         `firestore:"app_name"`
	UserID    string         `firestore:"user_id"`
	SessionID string         `firestore:"session_id"`
	State     map[string]any `firestore:"state"`
	NextSeq   int64          `firestore:"next_seq"` // next event sequence number
	UpdatedAt time.Time      `firestore:"updated_at"`
}

// eventDoc is one event in a session's "events" sub-collection, ordered by Seq.
type eventDoc struct {
	Seq       int64     `firestore:"seq"`
	Timestamp time.Time `firestore:"timestamp"`
	Blob      string    `firestore:"blob"` // JSON-encoded session.Event
}

type stateDoc struct {
	State map[string]any `firestore:"state"`
}

// --- key encoding ---

// encodeKey builds a Firestore-safe document id from its parts (base64url of a
// delimiter-joined key), so arbitrary app/user/session ids cannot collide or contain
// illegal characters.
func encodeKey(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x1f")))
}

// --- state scope helpers (replicate internal/sessionutils) ---

func extractStateDeltas(delta map[string]any) (app, user, sess map[string]any) {
	app, user, sess = map[string]any{}, map[string]any{}, map[string]any{}
	for k, v := range delta {
		switch {
		case strings.HasPrefix(k, session.KeyPrefixApp):
			app[strings.TrimPrefix(k, session.KeyPrefixApp)] = v
		case strings.HasPrefix(k, session.KeyPrefixUser):
			user[strings.TrimPrefix(k, session.KeyPrefixUser)] = v
		case !strings.HasPrefix(k, session.KeyPrefixTemp):
			sess[k] = v
		}
	}
	return app, user, sess
}

func mergeStates(app, user, sess map[string]any) map[string]any {
	out := make(map[string]any, len(app)+len(user)+len(sess))
	for k, v := range sess {
		out[k] = v
	}
	for k, v := range app {
		out[session.KeyPrefixApp+k] = v
	}
	for k, v := range user {
		out[session.KeyPrefixUser+k] = v
	}
	return out
}

// --- collections ---

func (s *firestoreSessionService) sessionRef(app, user, sid string) *firestore.DocumentRef {
	return s.client.Collection(s.sessions).Doc(encodeKey(app, user, sid))
}

func (s *firestoreSessionService) appStateRef(app string) *firestore.DocumentRef {
	return s.client.Collection(s.appSt).Doc(encodeKey(app))
}

func (s *firestoreSessionService) userStateRef(app, user string) *firestore.DocumentRef {
	return s.client.Collection(s.userSt).Doc(encodeKey(app, user))
}

func loadState(ctx context.Context, ref *firestore.DocumentRef) (map[string]any, error) {
	snap, err := ref.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var d stateDoc
	if err := snap.DataTo(&d); err != nil {
		return nil, err
	}
	if d.State == nil {
		d.State = map[string]any{}
	}
	return d.State, nil
}

// loadStateTx reads a scoped state doc inside a transaction (so the read participates in the
// transaction's snapshot and conflict detection). A missing doc yields an empty map.
func loadStateTx(tx *firestore.Transaction, ref *firestore.DocumentRef) (map[string]any, error) {
	snap, err := tx.Get(ref)
	if status.Code(err) == codes.NotFound {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var d stateDoc
	if err := snap.DataTo(&d); err != nil {
		return nil, err
	}
	if d.State == nil {
		d.State = map[string]any{}
	}
	return d.State, nil
}

// --- Service ---

func (s *firestoreSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req.AppName == "" || req.UserID == "" {
		return nil, fmt.Errorf("app_name and user_id are required, got app_name: %q, user_id: %q", req.AppName, req.UserID)
	}
	sid := req.SessionID
	if sid == "" {
		sid = uuid.NewString()
	}
	appDelta, userDelta, sessDelta := extractStateDeltas(req.State)
	now := time.Now()
	ref := s.sessionRef(req.AppName, req.UserID, sid)
	appRef := s.appStateRef(req.AppName)
	userRef := s.userStateRef(req.AppName, req.UserID)

	// One transaction creates the session and merges app/user state together, so a state
	// write failure can no longer leave a session persisted without its state (or vice
	// versa). All reads precede all writes, as Firestore transactions require.
	var appState, userState map[string]any
	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if err == nil && snap.Exists() {
			return fmt.Errorf("session %s already exists", sid)
		}
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		if appState, err = loadStateTx(tx, appRef); err != nil {
			return err
		}
		if userState, err = loadStateTx(tx, userRef); err != nil {
			return err
		}
		for k, v := range appDelta {
			appState[k] = v
		}
		for k, v := range userDelta {
			userState[k] = v
		}
		if err := tx.Create(ref, sessionDoc{
			AppName: req.AppName, UserID: req.UserID, SessionID: sid,
			State: sessDelta, UpdatedAt: now,
		}); err != nil {
			return err
		}
		if len(appDelta) > 0 {
			if err := tx.Set(appRef, stateDoc{State: appState}); err != nil {
				return err
			}
		}
		if len(userDelta) > 0 {
			if err := tx.Set(userRef, stateDoc{State: userState}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &session.CreateResponse{Session: &fsSession{
		appName: req.AppName, userID: req.UserID, sessionID: sid,
		state: mergeStates(appState, userState, sessDelta), updated: now,
	}}, nil
}

func (s *firestoreSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return nil, fmt.Errorf("app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q", req.AppName, req.UserID, req.SessionID)
	}
	snap, err := s.sessionRef(req.AppName, req.UserID, req.SessionID).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, fmt.Errorf("session %s not found", req.SessionID)
	}
	if err != nil {
		return nil, err
	}
	sess, err := s.hydrate(ctx, snap)
	if err != nil {
		return nil, err
	}
	events, err := loadEvents(ctx, snap.Ref)
	if err != nil {
		return nil, err
	}
	sess.events = filterEvents(events, req.NumRecentEvents, req.After)
	return &session.GetResponse{Session: sess}, nil
}

// loadEvents reads a session's events sub-collection in sequence order.
func loadEvents(ctx context.Context, sessionRef *firestore.DocumentRef) ([]*session.Event, error) {
	docs, err := sessionRef.Collection("events").OrderBy("seq", firestore.Asc).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	events := make([]*session.Event, 0, len(docs))
	for _, snap := range docs {
		var ed eventDoc
		if err := snap.DataTo(&ed); err != nil {
			return nil, err
		}
		var ev session.Event
		if err := json.Unmarshal([]byte(ed.Blob), &ev); err != nil {
			return nil, fmt.Errorf("unmarshal event: %w", err)
		}
		events = append(events, &ev)
	}
	return events, nil
}

func (s *firestoreSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req.AppName == "" {
		return nil, fmt.Errorf("app_name is required, got app_name: %q", req.AppName)
	}
	q := s.client.Collection(s.sessions).Where("app_name", "==", req.AppName)
	if req.UserID != "" {
		q = q.Where("user_id", "==", req.UserID)
	}
	docs, err := q.Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(docs))
	for _, snap := range docs {
		sess, err := s.hydrate(ctx, snap)
		if err != nil {
			return nil, err
		}
		sess.events = nil // List returns sessions without their event history
		out = append(out, sess)
	}
	return &session.ListResponse{Sessions: out}, nil
}

func (s *firestoreSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return fmt.Errorf("app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q", req.AppName, req.UserID, req.SessionID)
	}
	ref := s.sessionRef(req.AppName, req.UserID, req.SessionID)
	// Firestore does not cascade: delete the events sub-collection before the session doc.
	evs, err := ref.Collection("events").Documents(ctx).GetAll()
	if err != nil {
		return err
	}
	for _, ev := range evs {
		if _, err := ev.Ref.Delete(ctx); err != nil {
			return err
		}
	}
	_, err = ref.Delete(ctx)
	return err
}

func (s *firestoreSessionService) AppendEvent(ctx context.Context, curSession session.Session, event *session.Event) error {
	if curSession == nil {
		return fmt.Errorf("session is nil")
	}
	if event == nil {
		return fmt.Errorf("event is nil")
	}
	if event.Partial {
		return nil // partial events are not persisted
	}
	sess, ok := curSession.(*fsSession)
	if !ok {
		return fmt.Errorf("unexpected session type %T", curSession)
	}

	appDelta, userDelta, sessDelta := extractStateDeltas(event.Actions.StateDelta)
	stored := trimTempState(event)
	blob, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	ref := s.sessionRef(sess.appName, sess.userID, sess.sessionID)
	appRef := s.appStateRef(sess.appName)
	userRef := s.userStateRef(sess.appName, sess.userID)
	// Merge app/user state in the same transaction as the session + event write, so the
	// scoped state can no longer advance without the event that produced it (or vice versa).
	// All reads precede all writes, as Firestore transactions require.
	err = s.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("session not found, cannot apply event")
		}
		if err != nil {
			return err
		}
		appState, err := loadStateTx(tx, appRef)
		if err != nil {
			return err
		}
		userState, err := loadStateTx(tx, userRef)
		if err != nil {
			return err
		}
		var d sessionDoc
		if err := snap.DataTo(&d); err != nil {
			return err
		}
		if d.State == nil {
			d.State = map[string]any{}
		}
		for k, v := range sessDelta {
			d.State[k] = v
		}
		seq := d.NextSeq
		d.NextSeq++
		d.UpdatedAt = event.Timestamp
		if err := tx.Set(ref, d); err != nil {
			return err
		}
		if len(appDelta) > 0 {
			for k, v := range appDelta {
				appState[k] = v
			}
			if err := tx.Set(appRef, stateDoc{State: appState}); err != nil {
				return err
			}
		}
		if len(userDelta) > 0 {
			for k, v := range userDelta {
				userState[k] = v
			}
			if err := tx.Set(userRef, stateDoc{State: userState}); err != nil {
				return err
			}
		}
		evRef := ref.Collection("events").Doc(fmt.Sprintf("%020d", seq))
		return tx.Set(evRef, eventDoc{Seq: seq, Timestamp: event.Timestamp, Blob: string(blob)})
	})
	if err != nil {
		return err
	}

	// Reflect the append on the caller's in-memory session, mirroring the in-memory service:
	// the full (temp-stripped) delta, so app:/user: prefixed keys are visible too.
	for k, v := range stored.Actions.StateDelta {
		sess.state[k] = v
	}
	sess.events = append(sess.events, stored)
	sess.updated = event.Timestamp
	return nil
}

// hydrate loads a stored session doc plus its app/user state into an *fsSession with the
// merged, scope-prefixed state the caller expects.
func (s *firestoreSessionService) hydrate(ctx context.Context, snap *firestore.DocumentSnapshot) (*fsSession, error) {
	var d sessionDoc
	if err := snap.DataTo(&d); err != nil {
		return nil, err
	}
	appState, err := loadState(ctx, s.appStateRef(d.AppName))
	if err != nil {
		return nil, err
	}
	userState, err := loadState(ctx, s.userStateRef(d.AppName, d.UserID))
	if err != nil {
		return nil, err
	}
	if d.State == nil {
		d.State = map[string]any{}
	}
	// Events are loaded separately (Get) or omitted (List); hydrate only resolves state.
	return &fsSession{
		appName: d.AppName, userID: d.UserID, sessionID: d.SessionID,
		state: mergeStates(appState, userState, d.State), updated: d.UpdatedAt,
	}, nil
}

// trimTempState returns a copy of the event with temp: state-delta keys removed (they are
// never persisted).
func trimTempState(event *session.Event) *session.Event {
	if len(event.Actions.StateDelta) == 0 {
		return event
	}
	filtered := make(map[string]any, len(event.Actions.StateDelta))
	for k, v := range event.Actions.StateDelta {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			filtered[k] = v
		}
	}
	cp := *event
	cp.Actions.StateDelta = filtered
	return &cp
}

// filterEvents applies the Get request's NumRecentEvents / After filters. Events are stored
// (and loaded) in sequence order, which is not guaranteed to be timestamp order — the caller
// sets each event's Timestamp — so the After filter scans linearly rather than binary-searching
// on a monotonicity it cannot assume.
func filterEvents(events []*session.Event, numRecent int, after time.Time) []*session.Event {
	if numRecent > 0 {
		if start := len(events) - numRecent; start > 0 {
			events = events[start:]
		}
	}
	if !after.IsZero() && len(events) > 0 {
		keep := len(events)
		for i, ev := range events {
			if !ev.Timestamp.Before(after) {
				keep = i
				break
			}
		}
		events = events[keep:]
	}
	return events
}

// --- session.Session / State / Events implementations ---

type fsSession struct {
	appName, userID, sessionID string
	state                      map[string]any
	events                     []*session.Event
	updated                    time.Time
}

func (s *fsSession) ID() string                { return s.sessionID }
func (s *fsSession) AppName() string           { return s.appName }
func (s *fsSession) UserID() string            { return s.userID }
func (s *fsSession) LastUpdateTime() time.Time { return s.updated }
func (s *fsSession) State() session.State      { return &fsState{m: s.state} }
func (s *fsSession) Events() session.Events    { return fsEvents(s.events) }

type fsState struct{ m map[string]any }

func (s *fsState) Get(key string) (any, error) {
	if v, ok := s.m[key]; ok {
		return v, nil
	}
	return nil, session.ErrStateKeyNotExist
}

func (s *fsState) Set(key string, value any) error {
	s.m[key] = value
	return nil
}

func (s *fsState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

type fsEvents []*session.Event

func (e fsEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}

func (e fsEvents) Len() int { return len(e) }

func (e fsEvents) At(i int) *session.Event {
	if i >= 0 && i < len(e) {
		return e[i]
	}
	return nil
}
