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
//
// ImageRef, ImageDigest, and SpecReferrerDigest are set by SetArtifact/
// SetReferrer as the build progresses. IntegrityHealth is a read-through
// snapshot of the reconcile axis (pkg/index.Entry.IntegrityHealth) taken at
// SetReferrer time — this package never computes or writes integrity_health
// itself; only the reconcile loop's SetIntegrityHealth is authoritative.
type Record struct {
	BuildID            string
	ToolSpecDigest     string
	Status             Status
	FailureReason      string
	ImageRef           string
	ImageDigest        string
	SpecReferrerDigest string
	IntegrityHealth    string
	RequestedAt        time.Time
	UpdatedAt          time.Time
}

// Store persists build state in SQLite.
type Store struct {
	db *sql.DB
}

// ErrNotFound is returned when a build record does not exist.
var ErrNotFound = errors.New("buildstate: record not found")

// ErrAlreadyTerminal is returned by Transition when the build has already
// reached a terminal status. This is a benign race (e.g. CancelToolBuild and
// the build's own goroutine both attempting a terminal transition), not a
// storage failure — callers should treat it differently from any other
// Transition error.
var ErrAlreadyTerminal = errors.New("buildstate: build already terminal")

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

// buildStateArtifactColumns are the columns added in Sprint 7. They're listed
// in CREATE TABLE for fresh databases and individually migrated via
// ensureColumn for databases created before this sprint.
var buildStateArtifactColumns = []string{"image_ref", "image_digest", "spec_referrer_digest", "integrity_health"}

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
	image_ref TEXT NOT NULL DEFAULT '',
	image_digest TEXT NOT NULL DEFAULT '',
	spec_referrer_digest TEXT NOT NULL DEFAULT '',
	integrity_health TEXT NOT NULL DEFAULT '',
	requested_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("buildstate migrate: %w", err)
	}
	for _, column := range buildStateArtifactColumns {
		if err := s.ensureColumn(ctx, column); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn adds column as a TEXT NOT NULL column (default empty string)
// to build_state if it doesn't already exist. This is a minimal, idempotent
// migration path for
// databases created before Sprint 7 — every column added this way is
// additive and default-safe, so no separate schema-version table is needed.
func (s *Store) ensureColumn(ctx context.Context, column string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(build_state)`)
	if err != nil {
		return fmt.Errorf("buildstate ensure column %q: table_info: %w", column, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if scanErr := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); scanErr != nil {
			return fmt.Errorf("buildstate ensure column %q: scan table_info: %w", column, scanErr)
		}
		if name == column {
			return nil // already present
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("buildstate ensure column %q: iterate table_info: %w", column, rowsErr)
	}

	// column always comes from the fixed buildStateArtifactColumns list above,
	// never from external input, so building the DDL string is safe here.
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE build_state ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, column),
	); err != nil {
		return fmt.Errorf("buildstate ensure column %q: alter table: %w", column, err)
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
		return Record{}, fmt.Errorf(
			"buildstate: build %q already terminal (%s): %w", buildID, current.Status, ErrAlreadyTerminal,
		)
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

// setColumns runs an UPDATE against build_state for buildID and returns the
// refreshed record, or ErrNotFound if buildID doesn't exist. Shared by
// SetArtifact and SetReferrer.
func (s *Store) setColumns(buildID, query string, args ...any) (Record, error) {
	if buildID == "" {
		return Record{}, errors.New("buildstate: buildID must not be empty")
	}
	res, err := s.db.ExecContext(context.Background(), query, args...)
	if err != nil {
		return Record{}, fmt.Errorf("buildstate update %q: %w", buildID, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return Record{}, ErrNotFound
	}
	return s.Get(buildID)
}

// SetArtifact records the pushed image's ref and digest once known — called
// after a successful build/push, before the spec referrer push attempt.
func (s *Store) SetArtifact(buildID, imageRef, imageDigest string, now time.Time) (Record, error) {
	return s.setColumns(buildID,
		`UPDATE build_state SET image_ref = ?, image_digest = ?, updated_at = ? WHERE build_id = ?`,
		imageRef, imageDigest, now.UTC().UnixMilli(), buildID,
	)
}

// SetReferrer records the spec referrer digest (empty on push failure) and a
// read-through snapshot of integrity_health. It does not compute or write
// integrity_health anywhere else — the value passed in must already have
// been produced by the reconcile loop's SetIntegrityHealth (the sole
// authority for that axis); this method only mirrors it for WatchToolBuild.
func (s *Store) SetReferrer(buildID, specReferrerDigest, integrityHealth string, now time.Time) (Record, error) {
	return s.setColumns(buildID,
		`UPDATE build_state SET spec_referrer_digest = ?, integrity_health = ?, updated_at = ? WHERE build_id = ?`,
		specReferrerDigest, integrityHealth, now.UTC().UnixMilli(), buildID,
	)
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

const recordColumns = `build_id, tool_spec_digest, status, failure_reason,
	image_ref, image_digest, spec_referrer_digest, integrity_health, requested_at, updated_at`

func getInDB(ctx context.Context, db *sql.DB, buildID string) (Record, error) {
	return scanRecord(db.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM build_state WHERE build_id = ?`, buildID,
	))
}

func getInTx(ctx context.Context, tx *sql.Tx, buildID string) (Record, error) {
	return scanRecord(tx.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM build_state WHERE build_id = ?`, buildID,
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
		&rec.BuildID, &rec.ToolSpecDigest, &status, &rec.FailureReason,
		&rec.ImageRef, &rec.ImageDigest, &rec.SpecReferrerDigest, &rec.IntegrityHealth,
		&requestedAt, &updatedAt,
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
