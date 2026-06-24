package setup

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/session/database"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
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
// agent can be driven against a durable backend.
func newDurableCIWaiter(t *testing.T, appName string, svc session.Service) *runner.Runner {
	t.Helper()
	awaitCI, err := functiontool.New(functiontool.Config{
		Name:          "await_ci",
		Description:   "Open the PR and wait for CI to report.",
		IsLongRunning: true,
	}, func(_ tool.Context, _ struct {
		PR int `json:"pr"`
	}) (map[string]any, error) {
		return map[string]any{"status": "pending"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := llmagent.New(llmagent.Config{
		Name:        "ci-waiter",
		Model:       suspendStub{},
		Instruction: "Call await_ci and report the result.",
		Tools:       []tool.Tool{awaitCI},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: svc, AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDurableCrossProcessResume is the spike that gates the durable-sessions work: it proves
// adk-go's session/database backend round-trips a *long-running park* across what is
// effectively a process restart.
//
// A run parks on await_ci against a SQLite file; the runner + session service are then
// discarded and rebuilt from scratch over the SAME file, and the parked call is resumed
// with its CI result. If the run concludes (rather than restarting at triage or failing
// to find the parked call), durable suspend/resume in adk-go is real and Design A is
// safe to build on.
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
