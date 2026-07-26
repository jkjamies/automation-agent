package setup

import (
	"context"
	"io"
	"os"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
)

// TestFirestoreSessionConformance runs adk's own session.Service conformance suite against
// the Firestore-backed service (emulator-gated). Passing it proves the custom service
// honors the full contract: create/get/list/delete, partial-event skipping, event filters,
// and the app:/user:/temp: state scopes.
func TestFirestoreSessionConformance(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST to run the firestore session conformance suite")
	}
	opts := sessiontestsuite.SuiteOptions{SupportsUserProvidedSessionID: true}
	sessiontestsuite.RunServiceTests(t, opts, func(t *testing.T) session.Service {
		// A per-run-unique prefix isolates cases on the shared, persistent emulator.
		svc, err := NewFirestoreSessionService(context.Background(), "test-project", firestorePrefix("conf"))
		if err != nil {
			t.Fatalf("new firestore session service: %v", err)
		}
		if c, ok := svc.(io.Closer); ok {
			t.Cleanup(func() { _ = c.Close() })
		}
		return svc
	})
}

// TestFirestoreSession_AppendEvent_WorkflowFieldsRoundTrip proves the JSON-blob event
// encoding persists the workflow-execution fields (NodeInfo, RequestedInput, Routes,
// Output, IsolationScope). A paused workflow run is reconstructed entirely from these
// persisted events, so a field this store dropped would break resume silently — this
// test is the guard.
func TestFirestoreSession_AppendEvent_WorkflowFieldsRoundTrip(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST to run the firestore workflow-fields round-trip test")
	}
	ctx := context.Background()
	svc, err := NewFirestoreSessionService(ctx, "test-project", firestorePrefix("wf"))
	if err != nil {
		t.Fatalf("new firestore session service: %v", err)
	}
	if c, ok := svc.(io.Closer); ok {
		t.Cleanup(func() { _ = c.Close() })
	}

	created, err := svc.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := created.Session

	const wantScope = "adk-task-isolation-scope"
	event := &session.Event{
		ID:             "wf_event",
		Author:         "agent",
		IsolationScope: wantScope,
		NodeInfo:       &session.NodeInfo{Path: "await_ci"},
		RequestedInput: &session.RequestInput{InterruptID: "await_ci-inv1", Message: "waiting on CI"},
		Routes:         []string{"failure"},
		Output:         map[string]any{"pr_number": float64(7)},
	}
	if err := svc.AppendEvent(ctx, sess, event); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	got, err := svc.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: sess.ID()})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	evs := got.Session.Events()
	if evs.Len() != 1 {
		t.Fatalf("got %d events, want 1", evs.Len())
	}
	ev := evs.At(0)
	if ev.NodeInfo == nil || ev.NodeInfo.Path != "await_ci" {
		t.Errorf("NodeInfo not persisted: %#v", ev.NodeInfo)
	}
	if ev.RequestedInput == nil || ev.RequestedInput.InterruptID != "await_ci-inv1" {
		t.Errorf("RequestedInput not persisted: %#v", ev.RequestedInput)
	}
	if len(ev.Routes) != 1 || ev.Routes[0] != "failure" {
		t.Errorf("Routes not persisted: %#v", ev.Routes)
	}
	if ev.IsolationScope != wantScope {
		t.Errorf("IsolationScope = %q, want %q", ev.IsolationScope, wantScope)
	}
	out, ok := ev.Output.(map[string]any)
	if !ok || out["pr_number"] != float64(7) {
		t.Errorf("Output not persisted generically: %#v", ev.Output)
	}
}
