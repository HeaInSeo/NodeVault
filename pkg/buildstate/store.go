// Package buildstate provides durable state for asynchronous tool builds.
package buildstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Status is the durable lifecycle state of one build execution.
type Status string

const (
	StatusRequested   Status = "Requested"
	StatusResolving   Status = "Resolving"
	StatusBuilding    Status = "Building"
	StatusPushing     Status = "Pushing"
	StatusSucceeded   Status = "Succeeded"
	StatusFailed      Status = "Failed"
	StatusInterrupted Status = "Interrupted"
)

// Record is the persisted state of one submitted build.
type Record struct {
	BuildID        string
	ToolSpecDigest string
	Status         Status
	FailureReason  string
	RequestedAt    time.Time
	UpdatedAt      time.Time
}

// Store persists build state in SQLite.
type Store struct {
	db *sql.DB
}

// ErrNotFound is returned when a build record does not exist.
var ErrNotFound = errors.New("buildstate: record not found")

// Open creates or opens a SQLite build state database at path.
func Open(path string) (*Store, error) {
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o750); err != nil {
		return nil, fmt.Errorf("buildstate mkdir: %w", err)
	}
	db, err := sql.Open("sqlite3", cleanPath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("buildstate open: %w", err)
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init() error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("buildstate enable WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS build_state (
	build_id TEXT PRIMARY KEY,
	tool_spec_digest TEXT NOT NULL,
	status TEXT NOT NULL,
	failure_reason TEXT NOT NULL DEFAULT '',
	requested_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("buildstate migrate: %w", err)
	}
	return nil
}

// Create inserts a new build in Requested state.
func (s *Store) Create(buildID, toolSpecDigest string, now time.Time) (Record, error) {
	if buildID == "" {
		return Record{}, errors.New("buildstate: buildID must not be empty")
	}
	if toolSpecDigest == "" {
		return Record{}, errors.New("buildstate: toolSpecDigest must not be empty")
	}
	now = now.UTC()
	rec := Record{
		BuildID:        buildID,
		ToolSpecDigest: toolSpecDigest,
		Status:         StatusRequested,
		RequestedAt:    now,
		UpdatedAt:      now,
	}
	if _, err := s.db.ExecContext(
		context.Background(),
		`INSERT INTO build_state (build_id, tool_spec_digest, status, failure_reason, requested_at, updated_at)
VALUES (?, ?, ?, '', ?, ?)`,
		rec.BuildID, rec.ToolSpecDigest, rec.Status, rec.RequestedAt.UnixMilli(), rec.UpdatedAt.UnixMilli(),
	); err != nil {
		return Record{}, fmt.Errorf("buildstate create %q: %w", buildID, err)
	}
	return rec, nil
}

// CreateOrGet creates a submitted build or returns the existing record for an
// idempotent retry. The caller must reject a retry that changes the digest.
func (s *Store) CreateOrGet(buildID, toolSpecDigest string, now time.Time) (Record, bool, error) {
	rec, err := s.Create(buildID, toolSpecDigest, now)
	if err == nil {
		return rec, true, nil
	}
	existing, getErr := s.Get(buildID)
	if getErr != nil {
		return Record{}, false, err
	}
	if existing.ToolSpecDigest != toolSpecDigest {
		return Record{}, false, fmt.Errorf("buildstate: build %q already belongs to a different tool spec", buildID)
	}
	return existing, false, nil
}

// Transition atomically moves one build to the next status.
func (s *Store) Transition(buildID string, next Status, failureReason string, now time.Time) (Record, error) {
	if buildID == "" {
		return Record{}, errors.New("buildstate: buildID must not be empty")
	}
	if !validStatus(next) {
		return Record{}, fmt.Errorf("buildstate: invalid status %q", next)
	}
	now = now.UTC()

	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("buildstate transition begin: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	current, err := getInTx(ctx, tx, buildID)
	if err != nil {
		return Record{}, err
	}
	if terminal(current.Status) {
		return Record{}, fmt.Errorf("buildstate: build %q already terminal: %s", buildID, current.Status)
	}

	if _, execErr := tx.ExecContext(
		ctx,
		`UPDATE build_state SET status = ?, failure_reason = ?, updated_at = ? WHERE build_id = ?`,
		next, failureReason, now.UnixMilli(), buildID,
	); execErr != nil {
		return Record{}, fmt.Errorf("buildstate transition %q: %w", buildID, execErr)
	}
	rec, err := getInTx(ctx, tx, buildID)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("buildstate transition commit: %w", err)
	}
	tx = nil
	return rec, nil
}

// RecoverInterrupted marks non-terminal builds as Interrupted after process restart.
func (s *Store) RecoverInterrupted(now time.Time) (int, error) {
	now = now.UTC()
	res, err := s.db.ExecContext(
		context.Background(),
		`UPDATE build_state
SET status = ?, failure_reason = ?, updated_at = ?
WHERE status IN (?, ?, ?, ?)`,
		StatusInterrupted, "nodevault process restarted before build reached a terminal state", now.UnixMilli(),
		StatusRequested, StatusResolving, StatusBuilding, StatusPushing,
	)
	if err != nil {
		return 0, fmt.Errorf("buildstate recover interrupted: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("buildstate recover interrupted rows: %w", err)
	}
	return int(count), nil
}

// Get returns one build record by id.
func (s *Store) Get(buildID string) (Record, error) {
	return getInDB(context.Background(), s.db, buildID)
}

func getInDB(ctx context.Context, db *sql.DB, buildID string) (Record, error) {
	return scanRecord(db.QueryRowContext(ctx,
		`SELECT build_id, tool_spec_digest, status, failure_reason, requested_at, updated_at
FROM build_state WHERE build_id = ?`, buildID,
	))
}

func getInTx(ctx context.Context, tx *sql.Tx, buildID string) (Record, error) {
	return scanRecord(tx.QueryRowContext(ctx,
		`SELECT build_id, tool_spec_digest, status, failure_reason, requested_at, updated_at
FROM build_state WHERE build_id = ?`, buildID,
	))
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner) (Record, error) {
	var rec Record
	var status string
	var requestedAt int64
	var updatedAt int64
	if err := row.Scan(
		&rec.BuildID, &rec.ToolSpecDigest, &status, &rec.FailureReason, &requestedAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("buildstate scan: %w", err)
	}
	rec.Status = Status(status)
	rec.RequestedAt = time.UnixMilli(requestedAt).UTC()
	rec.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return rec, nil
}

// Terminal reports whether no further lifecycle transition is allowed.
func Terminal(status Status) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusInterrupted
}

func terminal(status Status) bool { return Terminal(status) }

func validStatus(status Status) bool {
	switch status {
	case StatusRequested, StatusResolving, StatusBuilding, StatusPushing, StatusSucceeded, StatusFailed, StatusInterrupted:
		return true
	default:
		return false
	}
}
