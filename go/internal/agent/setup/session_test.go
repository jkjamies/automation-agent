package setup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"automation-agent/internal/config"
)

// TestNewSessionServiceMemory: the default backend yields a usable in-memory service.
func TestNewSessionServiceMemory(t *testing.T) {
	svc, err := NewSessionService(context.Background(), config.Config{SessionBackend: config.SessionMemory})
	if err != nil {
		t.Fatalf("memory backend: %v", err)
	}
	if svc == nil {
		t.Fatal("memory backend returned a nil service")
	}
}

// TestNewSessionServiceSQLite: the sqlite backend constructs + migrates over a real file.
func TestNewSessionServiceSQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "sessions.db")
	svc, err := NewSessionService(context.Background(), config.Config{SessionBackend: config.SessionSQLite, SQLiteDSN: dsn})
	if err != nil {
		t.Fatalf("sqlite backend: %v", err)
	}
	if svc == nil {
		t.Fatal("sqlite backend returned a nil service")
	}
}

// TestNewSessionServiceFirestore: the firestore backend constructs against the emulator.
func TestNewSessionServiceFirestore(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST to run the firestore session backend test")
	}
	svc, err := NewSessionService(context.Background(), config.Config{
		SessionBackend: config.SessionFirestore, FirestoreProject: "test-project", FirestoreCollection: "test_sess",
	})
	if err != nil {
		t.Fatalf("firestore backend: %v", err)
	}
	if svc == nil {
		t.Fatal("firestore backend returned a nil service")
	}
	// The Firestore-backed service owns a client; close it so the test leaks no resources.
	if closer, ok := svc.(io.Closer); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}
}

// TestNewSessionServiceUnknown: an unrecognized backend is rejected.
func TestNewSessionServiceUnknown(t *testing.T) {
	if _, err := NewSessionService(context.Background(), config.Config{SessionBackend: "redis"}); err == nil {
		t.Fatal("expected an error for an unknown backend")
	}
}
