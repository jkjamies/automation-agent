package setup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jkjamies/automation-agent/internal/config"
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

// TestNewSessionServiceFirestoreNotYet: firestore is a recognized backend whose impl
// lands in Phase B; until then it returns a clear not-implemented error rather than a
// nil service.
func TestNewSessionServiceFirestoreNotYet(t *testing.T) {
	_, err := NewSessionService(context.Background(), config.Config{SessionBackend: config.SessionFirestore})
	if err == nil {
		t.Fatal("expected a not-implemented error for the firestore backend")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error = %v, want a not-implemented message", err)
	}
}

// TestNewSessionServiceUnknown: an unrecognized backend is rejected.
func TestNewSessionServiceUnknown(t *testing.T) {
	if _, err := NewSessionService(context.Background(), config.Config{SessionBackend: "redis"}); err == nil {
		t.Fatal("expected an error for an unknown backend")
	}
}
