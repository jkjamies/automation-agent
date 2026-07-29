package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// parkRow is the gorm model backing the sqlite ParkStore. The pr_key column doubles as
// the resume index ("" when the run is not parked); making it the column rather than a
// separate map means re-parking under a new key cannot leak a stale index entry. The
// workflow column scopes every claim to the owning engine (see ParkRecord.Workflow), so
// the index is really (workflow, pr_key).
type parkRow struct {
	SessionID string `gorm:"primaryKey"`
	Workflow  string `gorm:"index:idx_workflow_pr_key,priority:1"`
	PRKey     string `gorm:"index:idx_workflow_pr_key,priority:2"`
	CallID    string
	Attempts  int
	Params    string
	// autoUpdateTime:false is load-bearing, not decoration. `UpdatedAt` is a magic field
	// name to gorm, which otherwise overwrites it with the current time on every save —
	// silently discarding the caller's value, so a record could never be written as already
	// stale and this backend would diverge from memory/firestore. The conformance suite
	// catches it if the tag is ever dropped.
	UpdatedAt time.Time `gorm:"autoUpdateTime:false"`
}

func (parkRow) TableName() string { return "parked_runs" }

// parkRow mirrors ParkRecord field for field (only the gorm tags differ), so these are
// plain conversions. Adding a field to one and not the other is then a compile error rather
// than a field silently dropped on the way to or from storage.
func (r parkRow) toRecord() ParkRecord { return ParkRecord(r) }

func rowFromRecord(r ParkRecord) parkRow { return parkRow(r) }

// sqliteParkStore persists park records to a sqlite file so they survive a restart. It is
// the park-record counterpart of the sqlite session backend and shares its DSN/file.
type sqliteParkStore struct {
	db *gorm.DB
}

// newSQLiteParkStore opens (and migrates) a park store over the sqlite file at dsn.
func newSQLiteParkStore(dsn string) (ParkStore, error) {
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		PrepareStmt: true,
		Logger:      logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite park store: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("sqlite park store db handle: %w", err)
	}
	// SQLite serializes writers; a single pooled connection makes the claim CAS and
	// Put/Sweep contention-free within the process, WAL lets the separate session pool read
	// the shared file without blocking, and busy_timeout makes any cross-pool write wait
	// rather than fail with SQLITE_BUSY.
	sqlDB.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if err := db.Exec(pragma).Error; err != nil {
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if err := db.AutoMigrate(&parkRow{}); err != nil {
		return nil, fmt.Errorf("migrate parked_runs: %w", err)
	}
	return &sqliteParkStore{db: db}, nil
}

func (s *sqliteParkStore) Put(ctx context.Context, r ParkRecord) error {
	db := s.db.WithContext(ctx)
	if r.PRKey != "" {
		// One active row per (workflow, pr_key): clear it on any OTHER session still holding it,
		// so resolve/sweep have a single winner (the index is non-unique). Scoped by workflow so
		// a sibling engine parked on the same PR number is left alone.
		if err := db.Model(&parkRow{}).
			Where("workflow = ? AND pr_key = ? AND session_id <> ?", r.Workflow, r.PRKey, r.SessionID).
			Update("pr_key", "").Error; err != nil {
			return err
		}
	}
	// Upsert by primary key (session id). Save rewrites every column, so the pr_key index
	// follows the record automatically.
	row := rowFromRecord(r)
	return db.Save(&row).Error
}

func (s *sqliteParkStore) Get(ctx context.Context, sessionID string) (ParkRecord, bool, error) {
	var row parkRow
	err := s.db.WithContext(ctx).First(&row, "session_id = ?", sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ParkRecord{}, false, nil
	}
	if err != nil {
		return ParkRecord{}, false, err
	}
	return row.toRecord(), true, nil
}

func (s *sqliteParkStore) ResolveByPRKey(ctx context.Context, workflow, prKey string) (ParkRecord, bool, error) {
	if prKey == "" {
		return ParkRecord{}, false, nil // an empty key would match unparked rows (pr_key='')
	}
	db := s.db.WithContext(ctx)
	var row parkRow
	err := db.First(&row, "workflow = ? AND pr_key = ?", workflow, prKey).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ParkRecord{}, false, nil
	}
	if err != nil {
		return ParkRecord{}, false, err
	}
	return s.claim(db, row)
}

func (s *sqliteParkStore) Sweep(ctx context.Context, workflow string, cutoff time.Time) ([]ParkRecord, error) {
	db := s.db.WithContext(ctx)
	var rows []parkRow
	if err := db.Where("workflow = ? AND pr_key <> '' AND updated_at < ?", workflow, cutoff).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ParkRecord, 0, len(rows))
	var errs []error
	for _, row := range rows {
		// Claim each candidate; a per-row error skips that row (it stays parked for the next
		// sweep) rather than discarding the rows already claimed this pass.
		rec, ok, err := s.claimStale(db, row, cutoff)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			rec.PRKey = row.PRKey // restore for the caller (timeout sweep needs the PR)
			out = append(out, rec)
		}
	}
	return out, errors.Join(errs...)
}

// claim is the atomic compare-and-swap that backs ResolveByPRKey: a conditional UPDATE
// clears pr_key only while it is still set, so of N concurrent claimers exactly one (the
// writer SQLite lets through first) gets RowsAffected==1; the rest see 0 and no-op. The
// per-run row is retained (only pr_key is cleared) so a retry can still read its params.
// The row was already selected by (workflow, pr_key), and session_id is the primary key,
// so this CAS is inherently workflow-scoped.
func (s *sqliteParkStore) claim(db *gorm.DB, row parkRow) (ParkRecord, bool, error) {
	return execClaim(db.Model(&parkRow{}).
		Where("session_id = ? AND pr_key = ?", row.SessionID, row.PRKey), row)
}

// claimStale is the sweep's CAS: like claim, but also requires parked_at < cutoff, so a
// row that was resolved and re-parked (fresh) after the scan is left alone rather than
// cleared with a false timeout.
func (s *sqliteParkStore) claimStale(db *gorm.DB, row parkRow, cutoff time.Time) (ParkRecord, bool, error) {
	return execClaim(db.Model(&parkRow{}).
		Where("session_id = ? AND pr_key = ? AND updated_at < ?", row.SessionID, row.PRKey, cutoff), row)
}

func execClaim(q *gorm.DB, row parkRow) (ParkRecord, bool, error) {
	res := q.Update("pr_key", "")
	if res.Error != nil {
		return ParkRecord{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return ParkRecord{}, false, nil
	}
	row.PRKey = ""
	return row.toRecord(), true, nil
}

func (s *sqliteParkStore) Delete(ctx context.Context, sessionID string) error {
	return s.db.WithContext(ctx).Delete(&parkRow{}, "session_id = ?", sessionID).Error
}

func (s *sqliteParkStore) SweepOrphans(ctx context.Context, workflow string, cutoff time.Time) ([]ParkRecord, error) {
	var rows []parkRow
	if err := s.db.WithContext(ctx).
		Where("workflow = ? AND pr_key = '' AND updated_at < ?", workflow, cutoff).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ParkRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.toRecord())
	}
	return out, nil
}

func (s *sqliteParkStore) ParkedCount(ctx context.Context, workflow string) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&parkRow{}).
		Where("workflow = ? AND pr_key <> ''", workflow).Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}
