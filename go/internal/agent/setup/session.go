package setup

import (
	"context"
	"fmt"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"google.golang.org/adk/session"
	"google.golang.org/adk/session/database"

	"github.com/jkjamies/automation-agent/internal/config"
)

// NewSessionService builds the ADK session service for the configured backend. The
// session service holds the durable suspend/resume history that lets a parked fix run
// continue after a process restart. Keeping the sqlite/gorm imports here respects the
// ARCH boundary (infrastructure SDKs live under internal/agent/setup).
//
//	memory    -> in-process; tests and ephemeral local runs (today's behavior, default)
//	sqlite    -> file-backed via adk session/database; durable local runs
//	firestore -> cloud, via the custom Firestore-backed service; durable cloud runs
//
// Only the long-running fix loop needs durability; ephemeral one-shot runners
// (explore/analyze/triage) keep using an in-memory session via NewRunner.
func NewSessionService(ctx context.Context, cfg config.Config) (session.Service, error) {
	switch cfg.SessionBackend {
	case config.SessionMemory:
		return session.InMemoryService(), nil
	case config.SessionSQLite:
		// Silent GORM logger: the get-or-create path issues benign "record not found"
		// lookups for unset app/user state. Real failures still propagate as the
		// returned errors from the service methods, independent of this logger.
		svc, err := database.NewSessionService(sqlite.Open(cfg.SQLiteDSN), &gorm.Config{
			PrepareStmt: true,
			Logger:      logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			return nil, fmt.Errorf("sqlite session service: %w", err)
		}
		if err := database.AutoMigrate(svc); err != nil {
			return nil, fmt.Errorf("sqlite session migrate: %w", err)
		}
		return svc, nil
	case config.SessionFirestore:
		return NewFirestoreSessionService(ctx, cfg.FirestoreProject, cfg.FirestoreCollection)
	default:
		return nil, fmt.Errorf("unknown session backend %q", cfg.SessionBackend)
	}
}
