package buildstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib" // also registers the "pgx" database/sql driver
)

// ProfileJ2Postgres names the PostgreSQL J2 build_state store (SP09-TX1 r1 N1). It is opened
// only through OpenPostgres with an explicit DSN and never falls back to the SQLite store.
const ProfileJ2Postgres = "j2-postgres"

// ErrStaleFence is returned when a mutation's owner or lease generation is not the build's
// current one, e.g. after another replica reclaimed the build. Nothing is written.
var ErrStaleFence = errors.New("buildstate: stale owner fence")

// ErrVersionConflict is returned when a mutation's expected version is not the stored one.
// Nothing is written.
var ErrVersionConflict = errors.New("buildstate: expected version conflict")

// ErrLeaseExpired is returned when the owner's lease has expired by the database clock. The
// owner may RenewLease while nobody has reclaimed the build. Nothing is written.
var ErrLeaseExpired = errors.New("buildstate: owner lease expired")

// ErrLeaseHeld is returned by Reclaim while the current owner's lease is still live.
var ErrLeaseHeld = errors.New("buildstate: owner lease still live")

// ErrDigestConflict is returned when a build ID is reused for a different tool spec digest.
var ErrDigestConflict = errors.New("buildstate: build already belongs to a different tool spec")

// ErrArtifactConflict is returned when an artifact reference that is already recorded would be
// replaced by a different value.
var ErrArtifactConflict = errors.New("buildstate: artifact reference already recorded with a different value")

// ErrUnfencedRecovery is returned by PostgresStore.RecoverInterrupted. Marking every
// non-terminal build Interrupted would also interrupt builds that live replicas still own;
// the J2 store recovers only expired leases through RecoverExpired (TX1-04).
var ErrUnfencedRecovery = errors.New("buildstate: unfenced recovery is unsupported by the J2 store")

// ErrCommitOutcomeUnknown is returned when COMMIT returned but the server warned while it was
// in flight, e.g. a canceled synchronous-replication wait, or when COMMIT failed in a way that
// can follow a durable commit, e.g. the connection or context was lost before the reply. The
// transaction may be committed (locally, or without the required standby acknowledgement);
// callers must re-read with Get before
// retrying and must not treat it as success (TX1-03). It is not retried automatically.
var ErrCommitOutcomeUnknown = errors.New("buildstate: postgres commit outcome unknown")

// ErrRestoreActivationHold is returned by every PostgresStore mutation when the store's
// activation does not belong to the database it runs on (restore, promoted standby) or is not
// active. Nothing is written; reads keep working (TX1-04/TX1-07 (8)).
var ErrRestoreActivationHold = errors.New(
	"buildstate: store activation hold: restore/failover activation evidence required")

// Fence is the write authority over one build: the owner that holds its lease, the lease
// generation, and the record version a mutation expects. Every mutation returns the fence for
// the next one. A Fence returned by Get is an observation, not authority to write.
type Fence struct {
	Owner      string
	Generation int64
	Version    int64
}

// pgSchemaVersion is the schema this binary writes. A newer stamp is refused before any
// mutation.
const pgSchemaVersion = 1

// maxSerializationRetries bounds the whole-transaction retries on serialization failure or
// deadlock; the context deadline bounds them too.
const maxSerializationRetries = 16

// recoverExpiredReason is the failure reason RecoverExpired records. The build's external
// side effects (registry push) are not reconciled by this store.
const recoverExpiredReason = "owner lease expired before the build reached a terminal state; " +
	"external build/push side effects need backend reconciliation"

// PostgresStore is the J2 build_state store: every mutation is one SERIALIZABLE transaction on
// one PostgreSQL primary that checks the store activation and the build's fence. Identities and
// caller strings are stored as bytea, so they keep their exact bytes with no collation, case
// folding or trimming. Lease expiry is judged by the database clock only.
type PostgresStore struct {
	db *sql.DB
	// watches maps a *pgconn.PgConn whose COMMIT is in flight to its *commitWatch.
	watches sync.Map
}

// OpenPostgres opens the store at dsn and applies its schema. An empty DSN or an unreachable
// database is an error; there is no fallback store.
func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("buildstate: DSN is required for the j2-postgres profile")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The parse error can echo the DSN, credentials included; do not wrap it.
		return nil, errors.New("buildstate: invalid postgres DSN")
	}
	s := &PostgresStore{}
	cfg.OnNotice = s.onNotice
	s.db = stdlib.OpenDB(*cfg)
	if err := s.db.PingContext(ctx); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("buildstate: postgres store unavailable: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("buildstate: migrate postgres store: %w", err)
	}
	return s, nil
}

// Close closes the underlying connection pool.
func (s *PostgresStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	return s.retryTx(ctx, func(tx *sql.Tx) error {
		// Serialize concurrent migrations of independent processes.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(5172093861)`); err != nil {
			return err
		}
		var hasMeta, hasStoreTables bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass('nv_buildstate_schema_meta') IS NOT NULL,
			(to_regclass('nv_build_state') IS NOT NULL OR to_regclass('nv_buildstate_activation') IS NOT NULL)`,
		).Scan(&hasMeta, &hasStoreTables); err != nil {
			return fmt.Errorf("inspect schema: %w", err)
		}
		// The stamp is the schema_version row, written in the same transaction as the tables.
		var current string
		stamped := false
		if hasMeta {
			err := tx.QueryRowContext(ctx,
				`SELECT value FROM nv_buildstate_schema_meta WHERE key = 'schema_version'`).Scan(&current)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("read schema version: %w", err)
			default:
				stamped = true
			}
		}
		// Store tables without this store's stamp were not created by this binary; adopting
		// them could reinterpret foreign rows (TX1-07 (5)).
		if hasStoreTables && !stamped {
			return errors.New("store tables exist without a schema stamp (foreign schema); refusing to open")
		}
		if stamped {
			v, perr := strconv.Atoi(current)
			if perr != nil || v < 1 || strconv.Itoa(v) != current {
				return fmt.Errorf("unreadable schema version %q; refusing to open", current)
			}
			if v > pgSchemaVersion {
				return fmt.Errorf("store schema version %d is newer than this binary's %d; refusing to open",
					v, pgSchemaVersion)
			}
		}
		for _, stmt := range []string{
			`CREATE TABLE IF NOT EXISTS nv_buildstate_schema_meta (key text PRIMARY KEY, value text NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS nv_build_state (
				build_id             bytea PRIMARY KEY CHECK (length(build_id) > 0),
				tool_spec_digest     bytea NOT NULL CHECK (length(tool_spec_digest) > 0),
				status               text NOT NULL CHECK (status IN
					('Requested','Resolving','Building','Pushing','Succeeded','Failed','Interrupted')),
				failure_reason       bytea NOT NULL,
				image_ref            bytea NOT NULL,
				image_digest         bytea NOT NULL,
				spec_referrer_digest bytea NOT NULL,
				integrity_health     bytea NOT NULL,
				owner_id             bytea NOT NULL CHECK (length(owner_id) > 0),
				lease_generation     bigint NOT NULL CHECK (lease_generation > 0),
				lease_expires_at     timestamptz NOT NULL,
				version              bigint NOT NULL CHECK (version > 0),
				requested_at         timestamptz NOT NULL,
				updated_at           timestamptz NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS nv_buildstate_activation (
				singleton         boolean PRIMARY KEY CHECK (singleton),
				epoch             bigint NOT NULL CHECK (epoch > 0),
				state             text NOT NULL,
				system_identifier text NOT NULL,
				timeline_id       bigint NOT NULL,
				database_oid      bigint NOT NULL)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO nv_buildstate_schema_meta (key, value)
			VALUES ('schema_version', $1) ON CONFLICT (key) DO NOTHING`, strconv.Itoa(pgSchemaVersion)); err != nil {
			return err
		}
		return activate(ctx, tx)
	})
}

// pgIdentityQuery reads the physical identity a store activation is bound to: the cluster's
// system identifier, the current WAL timeline (it changes on promotion and point-in-time
// restore) and the database OID (it changes on a logical restore into a new database).
const pgIdentityQuery = `SELECT
	(SELECT system_identifier::text FROM pg_control_system()),
	('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::bigint,
	(SELECT oid::bigint FROM pg_database WHERE datname = current_database())`

type pgIdentity struct {
	systemID   string
	timeline   int64
	databaseID int64
}

func readPGIdentity(ctx context.Context, tx *sql.Tx) (pgIdentity, error) {
	var id pgIdentity
	if err := tx.QueryRowContext(ctx, pgIdentityQuery).Scan(&id.systemID, &id.timeline, &id.databaseID); err != nil {
		return pgIdentity{}, fmt.Errorf("read store identity: %w", err)
	}
	return id, nil
}

const (
	storeStateActive      = "active"
	storeStateRestoreHold = "restore-hold"
)

// activate records the store activation on first open. An empty store is a clean bootstrap and
// becomes epoch 1, active, bound to this database's identity. A store that already holds builds
// but has no activation record (e.g. a partial restore) is recorded as held. An existing record
// is never rewritten: there is no automatic re-activation (TX1-04 RESTORE-ACTIVATION HOLD).
func activate(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM nv_buildstate_activation)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	var hasData bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM nv_build_state)`).Scan(&hasData); err != nil {
		return err
	}
	state := storeStateActive
	if hasData {
		state = storeStateRestoreHold
	}
	id, err := readPGIdentity(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO nv_buildstate_activation
		(singleton, epoch, state, system_identifier, timeline_id, database_oid) VALUES (true, 1, $1, $2, $3, $4)`,
		state, id.systemID, id.timeline, id.databaseID)
	return err
}

// requireActive runs inside every mutation transaction. The store is writable only when its
// activation record is active and bound to the database it runs on. A copy on another cluster,
// timeline or database is refused until activation is re-established with high-water and
// old-primary fencing evidence, which this slice does not implement. Raising the epoch alone
// does not lift the hold.
func requireActive(ctx context.Context, tx *sql.Tx) error {
	var state, systemID string
	var epoch, timeline, databaseID int64
	err := tx.QueryRowContext(ctx, `SELECT epoch, state, system_identifier, timeline_id, database_oid
		FROM nv_buildstate_activation WHERE singleton`).Scan(&epoch, &state, &systemID, &timeline, &databaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no activation record", ErrRestoreActivationHold)
	}
	if err != nil {
		return fmt.Errorf("read store activation: %w", err)
	}
	if state != storeStateActive {
		return fmt.Errorf("%w: epoch %d state %q", ErrRestoreActivationHold, epoch, state)
	}
	cur, err := readPGIdentity(ctx, tx)
	if err != nil {
		return err
	}
	if cur != (pgIdentity{systemID: systemID, timeline: timeline, databaseID: databaseID}) {
		return fmt.Errorf("%w: epoch %d was activated on system %s timeline %d database %d, "+
			"now on system %s timeline %d database %d",
			ErrRestoreActivationHold, epoch, systemID, timeline, databaseID, cur.systemID, cur.timeline, cur.databaseID)
	}
	return nil
}

// pgRow is one nv_build_state row with its fence and whether its lease is live by the
// database clock.
type pgRow struct {
	rec       Record
	fence     Fence
	leaseLive bool
}

const pgSelectRow = `SELECT build_id, tool_spec_digest, status, failure_reason, image_ref, image_digest,
	spec_referrer_digest, integrity_health, requested_at, updated_at,
	owner_id, lease_generation, version, lease_expires_at > clock_timestamp()
	FROM nv_build_state WHERE build_id = $1`

func scanPGRow(row rowScanner) (pgRow, error) {
	var buildID, digest, reason, imageRef, imageDigest, referrer, health, owner []byte
	var status string
	var r pgRow
	if err := row.Scan(&buildID, &digest, &status, &reason, &imageRef, &imageDigest, &referrer, &health,
		&r.rec.RequestedAt, &r.rec.UpdatedAt, &owner, &r.fence.Generation, &r.fence.Version, &r.leaseLive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pgRow{}, ErrNotFound
		}
		return pgRow{}, fmt.Errorf("buildstate scan: %w", err)
	}
	r.rec.BuildID, r.rec.ToolSpecDigest, r.rec.Status = string(buildID), string(digest), Status(status)
	r.rec.FailureReason, r.rec.ImageRef, r.rec.ImageDigest = string(reason), string(imageRef), string(imageDigest)
	r.rec.SpecReferrerDigest, r.rec.IntegrityHealth = string(referrer), string(health)
	r.rec.RequestedAt, r.rec.UpdatedAt = r.rec.RequestedAt.UTC(), r.rec.UpdatedAt.UTC()
	r.fence.Owner = string(owner)
	return r, nil
}

func lockRow(ctx context.Context, tx *sql.Tx, buildID string) (pgRow, error) {
	return scanPGRow(tx.QueryRowContext(ctx, pgSelectRow+` FOR UPDATE`, []byte(buildID)))
}

// Get returns one build record and its current fence, read from the primary. The fence is an
// observation only.
func (s *PostgresStore) Get(ctx context.Context, buildID string) (Record, Fence, error) {
	r, err := scanPGRow(s.db.QueryRowContext(ctx, pgSelectRow, []byte(buildID)))
	if err != nil {
		return Record{}, Fence{}, err
	}
	return r.rec, r.fence, nil
}

func checkIdentity(buildID, owner string) error {
	if buildID == "" {
		return errors.New("buildstate: buildID must not be empty")
	}
	if owner == "" {
		return errors.New("buildstate: owner must not be empty")
	}
	return nil
}

func checkTTL(ttl time.Duration) error {
	if ttl < time.Microsecond {
		return fmt.Errorf("buildstate: lease TTL %s must be at least 1µs", ttl)
	}
	return nil
}

// CreateOrGet creates a build in Requested state owned by owner with a lease of leaseTTL, or
// returns the existing record for an idempotent retry. A retry with a different tool spec
// digest is refused with ErrDigestConflict. A retry by the current owner (e.g. after a lost
// commit acknowledgement) gets the current fence back; any other caller gets a zero Fence and
// must not run the build.
func (s *PostgresStore) CreateOrGet(
	ctx context.Context, buildID, toolSpecDigest, owner string, leaseTTL time.Duration,
) (Record, Fence, bool, error) {
	if err := checkIdentity(buildID, owner); err != nil {
		return Record{}, Fence{}, false, err
	}
	if toolSpecDigest == "" {
		return Record{}, Fence{}, false, errors.New("buildstate: toolSpecDigest must not be empty")
	}
	if err := checkTTL(leaseTTL); err != nil {
		return Record{}, Fence{}, false, err
	}
	var out pgRow
	var created bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO nv_build_state (build_id, tool_spec_digest, status,
			failure_reason, image_ref, image_digest, spec_referrer_digest, integrity_health,
			owner_id, lease_generation, lease_expires_at, version, requested_at, updated_at)
			VALUES ($1, $2, $3, '', '', '', '', '', $4, 1,
				clock_timestamp() + $5 * interval '1 microsecond', 1, clock_timestamp(), clock_timestamp())
			ON CONFLICT (build_id) DO NOTHING`,
			[]byte(buildID), []byte(toolSpecDigest), string(StatusRequested), []byte(owner), leaseTTL.Microseconds())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n == 1
		out, err = lockRow(ctx, tx, buildID)
		return err
	})
	if err != nil {
		return Record{}, Fence{}, false, err
	}
	if out.rec.ToolSpecDigest != toolSpecDigest {
		return Record{}, Fence{}, false, fmt.Errorf("buildstate: build %q: %w", buildID, ErrDigestConflict)
	}
	if out.fence.Owner != owner {
		return out.rec, Fence{}, created, nil
	}
	return out.rec, out.fence, created, nil
}

// requireFence checks that f is the build's current owner, generation and version and that the
// lease is live by the database clock.
func requireFence(r *pgRow, f Fence) error {
	if r.fence.Owner != f.Owner || r.fence.Generation != f.Generation {
		return fmt.Errorf("buildstate: build %q is owned by generation %d: %w",
			r.rec.BuildID, r.fence.Generation, ErrStaleFence)
	}
	if r.fence.Version != f.Version {
		return fmt.Errorf("buildstate: build %q is at version %d, expected %d: %w",
			r.rec.BuildID, r.fence.Version, f.Version, ErrVersionConflict)
	}
	if !r.leaseLive {
		return fmt.Errorf("buildstate: build %q: %w", r.rec.BuildID, ErrLeaseExpired)
	}
	return nil
}

// replayed reports whether r is the result of the very mutation f was about to make: same owner
// and generation, one version later, and want(r) holds. A lost commit acknowledgement retried
// with the same fence then converges instead of failing the version check.
func replayed(r *pgRow, f Fence, want func(Record) bool) bool {
	return r.fence.Owner == f.Owner && r.fence.Generation == f.Generation &&
		r.fence.Version == f.Version+1 && want(r.rec)
}

// fencedUpdate locks the build, lets check refuse the mutation or report it as already applied,
// then runs update (which must bump version) and returns the new record and fence.
func (s *PostgresStore) fencedUpdate(
	ctx context.Context, buildID string,
	check func(r *pgRow) (applied bool, err error),
	update string, args ...any,
) (Record, Fence, error) {
	if buildID == "" {
		return Record{}, Fence{}, errors.New("buildstate: buildID must not be empty")
	}
	var out pgRow
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		r, err := lockRow(ctx, tx, buildID)
		if err != nil {
			return err
		}
		applied, err := check(&r)
		if err != nil {
			return err
		}
		if !applied {
			if _, err = tx.ExecContext(ctx, update, append([]any{[]byte(buildID)}, args...)...); err != nil {
				return err
			}
			if r, err = lockRow(ctx, tx, buildID); err != nil {
				return err
			}
		}
		out = r
		return nil
	})
	if err != nil {
		return Record{}, Fence{}, err
	}
	return out.rec, out.fence, nil
}

// Transition moves the build to next under fence f. A terminal build is refused with
// ErrAlreadyTerminal, which keeps the benign terminal race of the SQLite store.
func (s *PostgresStore) Transition(
	ctx context.Context, buildID string, f Fence, next Status, failureReason string,
) (Record, Fence, error) {
	if !validStatus(next) {
		return Record{}, Fence{}, fmt.Errorf("buildstate: invalid status %q", next)
	}
	return s.fencedUpdate(ctx, buildID, func(r *pgRow) (bool, error) {
		if replayed(r, f, func(rec Record) bool { return rec.Status == next && rec.FailureReason == failureReason }) {
			return true, nil
		}
		if terminal(r.rec.Status) {
			return false, fmt.Errorf("buildstate: build %q already terminal (%s): %w", buildID, r.rec.Status, ErrAlreadyTerminal)
		}
		return false, requireFence(r, f)
	}, `UPDATE nv_build_state SET status = $2, failure_reason = $3, version = version + 1,
		updated_at = clock_timestamp() WHERE build_id = $1`, string(next), []byte(failureReason))
}

// SetArtifact records the pushed image's ref and digest under fence f. They are immutable once
// recorded: the same values converge, different values are refused with ErrArtifactConflict.
func (s *PostgresStore) SetArtifact(
	ctx context.Context, buildID string, f Fence, imageRef, imageDigest string,
) (Record, Fence, error) {
	same := func(rec Record) bool { return rec.ImageRef == imageRef && rec.ImageDigest == imageDigest }
	return s.fencedUpdate(ctx, buildID, func(r *pgRow) (bool, error) {
		if replayed(r, f, same) {
			return true, nil
		}
		if err := requireFence(r, f); err != nil {
			return false, err
		}
		if r.rec.ImageRef != "" || r.rec.ImageDigest != "" {
			if same(r.rec) {
				return true, nil
			}
			return false, fmt.Errorf("buildstate: build %q image: %w", buildID, ErrArtifactConflict)
		}
		return false, nil
	}, `UPDATE nv_build_state SET image_ref = $2, image_digest = $3, version = version + 1,
		updated_at = clock_timestamp() WHERE build_id = $1`, []byte(imageRef), []byte(imageDigest))
}

// SetReferrer records the spec referrer digest and the integrity_health read-through snapshot
// under fence f. A recorded referrer digest is immutable; the snapshot may be refreshed.
func (s *PostgresStore) SetReferrer(
	ctx context.Context, buildID string, f Fence, specReferrerDigest, integrityHealth string,
) (Record, Fence, error) {
	same := func(rec Record) bool {
		return rec.SpecReferrerDigest == specReferrerDigest && rec.IntegrityHealth == integrityHealth
	}
	return s.fencedUpdate(ctx, buildID, func(r *pgRow) (bool, error) {
		if replayed(r, f, same) {
			return true, nil
		}
		if err := requireFence(r, f); err != nil {
			return false, err
		}
		if r.rec.SpecReferrerDigest != "" && r.rec.SpecReferrerDigest != specReferrerDigest {
			return false, fmt.Errorf("buildstate: build %q spec referrer: %w", buildID, ErrArtifactConflict)
		}
		return same(r.rec), nil
	}, `UPDATE nv_build_state SET spec_referrer_digest = $2, integrity_health = $3, version = version + 1,
		updated_at = clock_timestamp() WHERE build_id = $1`, []byte(specReferrerDigest), []byte(integrityHealth))
}

// RenewLease extends the lease of the owner that holds fence f to leaseTTL from now by the
// database clock. It works on an expired lease as long as nobody reclaimed the build. The
// version does not change, and f must carry the current version: a fence from before a later
// mutation is refused with ErrVersionConflict rather than renewed and handed the newer version.
func (s *PostgresStore) RenewLease(
	ctx context.Context, buildID string, f Fence, leaseTTL time.Duration,
) (Fence, error) {
	if err := checkTTL(leaseTTL); err != nil {
		return Fence{}, err
	}
	var out Fence
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		r, err := lockRow(ctx, tx, buildID)
		if err != nil {
			return err
		}
		if r.fence.Owner != f.Owner || r.fence.Generation != f.Generation {
			return fmt.Errorf("buildstate: build %q: %w", buildID, ErrStaleFence)
		}
		if r.fence.Version != f.Version {
			return fmt.Errorf("buildstate: build %q is at version %d, expected %d: %w",
				buildID, r.fence.Version, f.Version, ErrVersionConflict)
		}
		if terminal(r.rec.Status) {
			return fmt.Errorf("buildstate: build %q already terminal (%s): %w", buildID, r.rec.Status, ErrAlreadyTerminal)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nv_build_state
			SET lease_expires_at = clock_timestamp() + $2 * interval '1 microsecond' WHERE build_id = $1`,
			[]byte(buildID), leaseTTL.Microseconds()); err != nil {
			return err
		}
		out = r.fence
		return nil
	})
	return out, err
}

// Reclaim makes newOwner the owner of a non-terminal build whose lease has expired by the
// database clock. The lease generation and version are incremented, so every fence the previous
// owner holds is refused from then on. A live lease is refused with ErrLeaseHeld. Reclaim does
// not stop or undo the previous owner's external side effects.
func (s *PostgresStore) Reclaim(
	ctx context.Context, buildID, newOwner string, leaseTTL time.Duration,
) (Record, Fence, error) {
	if err := checkIdentity(buildID, newOwner); err != nil {
		return Record{}, Fence{}, err
	}
	if err := checkTTL(leaseTTL); err != nil {
		return Record{}, Fence{}, err
	}
	return s.fencedUpdate(ctx, buildID, func(r *pgRow) (bool, error) {
		if terminal(r.rec.Status) {
			return false, fmt.Errorf("buildstate: build %q already terminal (%s): %w", buildID, r.rec.Status, ErrAlreadyTerminal)
		}
		if r.leaseLive {
			return false, fmt.Errorf("buildstate: build %q: %w", buildID, ErrLeaseHeld)
		}
		return false, nil
	}, `UPDATE nv_build_state SET owner_id = $2, lease_generation = lease_generation + 1,
		lease_expires_at = clock_timestamp() + $3 * interval '1 microsecond', version = version + 1,
		updated_at = clock_timestamp() WHERE build_id = $1`, []byte(newOwner), leaseTTL.Microseconds())
}

// RecoverExpired reclaims, for recoverer, every non-terminal build whose lease has expired by
// the database clock and marks it Interrupted in the same statement. Builds whose owners still
// hold a live lease are not touched. Builds are not re-run, and the reason records that the
// external side effects still need backend reconciliation.
func (s *PostgresStore) RecoverExpired(ctx context.Context, recoverer string) (int, error) {
	if recoverer == "" {
		return 0, errors.New("buildstate: recoverer must not be empty")
	}
	var count int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE nv_build_state
			SET status = $1, failure_reason = $2, owner_id = $3, lease_generation = lease_generation + 1,
				lease_expires_at = clock_timestamp(), version = version + 1, updated_at = clock_timestamp()
			WHERE status IN ($4, $5, $6, $7) AND lease_expires_at <= clock_timestamp()`,
			string(StatusInterrupted), []byte(recoverExpiredReason), []byte(recoverer),
			string(StatusRequested), string(StatusResolving), string(StatusBuilding), string(StatusPushing))
		if err != nil {
			return err
		}
		count, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("buildstate recover expired: %w", err)
	}
	return int(count), nil
}

// RecoverInterrupted is the SQLite store's unfenced restart recovery. The J2 store refuses it
// with ErrUnfencedRecovery and writes nothing; use RecoverExpired.
func (*PostgresStore) RecoverInterrupted(time.Time) (int, error) {
	return 0, ErrUnfencedRecovery
}

// inTx runs a mutation: one SERIALIZABLE transaction that first checks the store activation.
func (s *PostgresStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.retryTx(ctx, func(tx *sql.Tx) error {
		if err := requireActive(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

// retryTx runs fn in one SERIALIZABLE transaction. On a serialization failure or deadlock the
// whole transaction is retried with the same frozen input, bounded by maxSerializationRetries
// and ctx. Any other error, including a failed COMMIT, is returned as is; an ambiguous COMMIT
// (ErrCommitOutcomeUnknown) is never retried.
func (s *PostgresStore) retryTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var err error
	for range maxSerializationRetries {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		err = s.tryTx(ctx, fn)
		if errors.Is(err, ErrCommitOutcomeUnknown) || !isRetryableSerialization(err) {
			return err
		}
	}
	return fmt.Errorf("buildstate: giving up after %d serialization retries: %w", maxSerializationRetries, err)
}

// tryTx runs one attempt on one pinned connection so that a WARNING the server sends while
// COMMIT is in flight is attributed to this transaction. PostgreSQL answers a canceled
// synchronous-replication wait with "COMMIT" plus a WARNING; that is not a durable
// acknowledgement, so it is reported as ErrCommitOutcomeUnknown. A COMMIT error that does not
// prove the transaction was rolled back (see commitOutcomeKnown) is reported the same way.
func (s *PostgresStore) tryTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var pg *pgconn.PgConn
	if rawErr := conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("buildstate: unexpected driver connection %T", driverConn)
		}
		pg = c.Conn().PgConn()
		return nil
	}); rawErr != nil {
		return rawErr
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	w := s.watch(pg)
	defer s.unwatch(pg)
	err = tx.Commit()
	if n := w.warning(); n != nil {
		return fmt.Errorf("%w: server warning during COMMIT: %s (%s)", ErrCommitOutcomeUnknown, n.Message, n.Detail)
	}
	if err != nil && !commitOutcomeKnown(err) {
		return fmt.Errorf("%w: COMMIT: %w", ErrCommitOutcomeUnknown, err)
	}
	return err
}

// commitOutcomeKnown reports whether a COMMIT error proves the transaction did not commit: the
// server rolled the transaction back or rejected the COMMIT with an ordinary SQL error, or pgx
// refused a closed transaction before any I/O. A lost connection, a canceled context or a
// server-side connection/shutdown/internal error can follow a durable commit, so those are
// ambiguous. pgconn.SafeToRetry is not used: pgconn reports "conn closed" as safe to retry
// even when the connection died after COMMIT was sent.
func commitOutcomeKnown(err error) bool {
	if errors.Is(err, pgx.ErrTxCommitRollback) || errors.Is(err, pgx.ErrTxClosed) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "08", "53", "57", "58", "XX": // connection, resources, operator intervention, system, internal
		return false
	}
	return true
}

// commitWatch records the first WARNING notice a connection receives while it is watched.
type commitWatch struct {
	mu   sync.Mutex
	seen *pgconn.Notice
}

func (w *commitWatch) note(n *pgconn.Notice) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = n
	}
}

func (w *commitWatch) warning() *pgconn.Notice {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

func (s *PostgresStore) watch(pg *pgconn.PgConn) *commitWatch {
	w := &commitWatch{}
	s.watches.Store(pg, w)
	return w
}

func (s *PostgresStore) unwatch(pg *pgconn.PgConn) { s.watches.Delete(pg) }

// onNotice is installed on every pooled connection. Notices outside a watched COMMIT are
// ignored.
func (s *PostgresStore) onNotice(pg *pgconn.PgConn, n *pgconn.Notice) {
	if !strings.EqualFold(n.SeverityUnlocalized, "WARNING") && !strings.EqualFold(n.Severity, "WARNING") {
		return
	}
	if w, ok := s.watches.Load(pg); ok {
		w.(*commitWatch).note(n)
	}
}

func isRetryableSerialization(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}
