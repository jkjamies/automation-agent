package setup

import (
	"context"
	"os"
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/adk/session/session_test"
)

// TestFirestoreSessionConformance runs adk's own session.Service conformance suite against
// the Firestore-backed service (emulator-gated). Passing it proves the custom service
// honors the full contract: create/get/list/delete, partial-event skipping, event filters,
// and the app:/user:/temp: state scopes.
func TestFirestoreSessionConformance(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST to run the firestore session conformance suite")
	}
	opts := session_test.SuiteOptions{SupportsUserProvidedSessionID: true}
	session_test.RunServiceTests(t, opts, func(t *testing.T) session.Service {
		// A per-run-unique prefix isolates cases on the shared, persistent emulator.
		svc, err := NewFirestoreSessionService(context.Background(), "test-project", firestorePrefix("conf"))
		if err != nil {
			t.Fatalf("new firestore session service: %v", err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		return svc
	})
}
