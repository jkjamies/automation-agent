package setup

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
)

// newSQLiteService builds a durable (file-backed) session service over dsn and runs the
// schema migration. It is the durable counterpart of session.InMemoryService() used by
// the in-memory suspend/resume tests, and is what a real local run would construct for
// SESSION_BACKEND=sqlite.
func newSQLiteService(t *testing.T, dsn string) session.Service {
	t.Helper()
	// Silent logger: the migration + get-or-create path logs benign "record not found"
	// lines for the unset app/user state rows, which would otherwise spam the test output.
	svc, err := database.NewSessionService(sqlite.Open(dsn), &gorm.Config{
		PrepareStmt: true,
		Logger:      logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("new sqlite session service: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("automigrate sqlite session schema: %v", err)
	}
	return svc
}

// newDurableCIWaiter mirrors newCIWaiter (suspend_resume_test.go) but runs over the
// supplied session service instead of an in-memory one, so the same await_ci parking
// workflow can be driven against a durable backend.
func newDurableCIWaiter(t *testing.T, appName string, svc session.Service) *runner.Runner {
	t.Helper()
	r, err := runner.New(runner.Config{AppName: appName, Agent: newCIWaiterAgent(t), SessionService: svc, AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDurableCrossProcessResume gates the durable-sessions design: it proves the
// session/database backend round-trips a *paused workflow run* across what is
// effectively a process restart — the paused graph state is rebuilt entirely from the
// persisted session events.
//
// A run parks on await_ci against a SQLite file; the runner + session service are then
// discarded and rebuilt from scratch over the SAME file, and the parked interrupt is
// resumed with its CI result. If the run concludes (rather than restarting or failing to
// find the pause), durable pause/resume is real and safe to build on.
func TestDurableCrossProcessResume(t *testing.T) {
	// busy_timeout lets the second connection wait briefly rather than fail if the first
	// pool (never explicitly closed) still holds the file.
	dsn := "file:" + filepath.Join(t.TempDir(), "sessions.db") + "?_pragma=busy_timeout(5000)"
	const appName, uid, sid = "susp", "u", "s"

	// "Process 1": drive to the await_ci park, then drop the runner + service so nothing
	// but the on-disk SQLite file carries the suspended run forward.
	var callID string
	func() {
		r := newDurableCIWaiter(t, appName, newSQLiteService(t, dsn))
		callID = park(t, r, uid, sid)
	}()
	t.Logf("parked on long-running call id=%q (process 1 torn down)", callID)

	// "Process 2": a brand-new service + runner over the same file resumes the park.
	r2 := newDurableCIWaiter(t, appName, newSQLiteService(t, dsn))
	final, reparked := resumeWith(t, r2, uid, sid, callID, "success")
	if reparked {
		t.Fatal("cross-process resume re-parked instead of concluding")
	}
	// Assert on the deterministic state transition (it concluded rather than re-parking),
	// not on the model's generated phrasing: a terminal response was produced.
	if final == "" {
		t.Fatal("cross-process resume produced no terminal response")
	}
	t.Logf("resumed across a simulated restart and concluded: %q", final)
}
