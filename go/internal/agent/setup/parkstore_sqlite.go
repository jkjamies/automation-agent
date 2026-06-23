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
// separate map means re-parking under a new key cannot leak a stale index entry.
type parkRow struct {
	SessionID string `gorm:"primaryKey"`
	PRKey     string `gorm:"index"`
	CallID    string
	Attempts  int
	Params    string
	ParkedAt  time.Time
}

func (parkRow) TableName() string { return "parked_runs" }

func (r parkRow) toRecord() ParkRecord {
	return ParkRecord{
		SessionID: r.SessionID, PRKey: r.PRKey, CallID: r.CallID,
		Attempts: r.Attempts, Params: r.Params, ParkedAt: r.ParkedAt,
	}
}

func rowFromRecord(r ParkRecord) parkRow {
	return parkRow{
		SessionID: r.SessionID, PRKey: r.PRKey, CallID: r.CallID,
		Attempts: r.Attempts, Params: r.Params, ParkedAt: r.ParkedAt,
	}
}

// sqliteParkStore persists park records to a sqlite file so they survive a restart. It is
// the park-record counterpart of the sqlite session backend and shares its DSN/file.
type sqliteParkStore struct {
	db *gorm.DB
}

// NewSQLiteParkStore opens (and migrates) a park store over the sqlite file at dsn.
func NewSQLiteParkStore(dsn string) (ParkStore, error) {
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
	// Upsert by primary key (session id). Save rewrites every column, so the pr_key index
	// follows the record automatically.
	row := rowFromRecord(r)
	return s.db.WithContext(ctx).Save(&row).Error
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

func (s *sqliteParkStore) ResolveByPRKey(ctx context.Context, prKey string) (ParkRecord, bool, error) {
	if prKey == "" {
		return ParkRecord{}, false, nil // an empty key would match unparked rows (pr_key='')
	}
	db := s.db.WithContext(ctx)
	var row parkRow
	err := db.First(&row, "pr_key = ?", prKey).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ParkRecord{}, false, nil
	}
	if err != nil {
		return ParkRecord{}, false, err
	}
	return s.claim(db, row)
}

func (s *sqliteParkStore) Sweep(ctx context.Context, cutoff time.Time) ([]ParkRecord, error) {
	db := s.db.WithContext(ctx)
	var rows []parkRow
	if err := db.Where("pr_key <> '' AND parked_at < ?", cutoff).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ParkRecord, 0, len(rows))
	for _, row := range rows {
		rec, ok, err := s.claim(db, row)
		if err != nil {
			return out, err
		}
		if ok {
			rec.PRKey = row.PRKey // restore for the caller (timeout sweep needs the PR)
			out = append(out, rec)
		}
	}
	return out, nil
}

// claim is the atomic compare-and-swap that backs ResolveByPRKey/Sweep: a conditional
// UPDATE clears pr_key only while it is still set, so of N concurrent claimers exactly one
// (the writer SQLite lets through first) gets RowsAffected==1; the rest see 0 and no-op.
// The per-run row is retained (only pr_key is cleared) so a retry can still read its params.
func (s *sqliteParkStore) claim(db *gorm.DB, row parkRow) (ParkRecord, bool, error) {
	res := db.Model(&parkRow{}).Where("session_id = ? AND pr_key = ?", row.SessionID, row.PRKey).Update("pr_key", "")
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

func (s *sqliteParkStore) ParkedCount(ctx context.Context) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&parkRow{}).Where("pr_key <> ''").Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}
