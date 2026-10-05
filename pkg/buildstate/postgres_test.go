package buildstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// pgTestDSNEnv points the PostgreSQL adapter tests at a disposable PostgreSQL 18.6 primary
// (TX1-CONFORMANCE). Unset, those tests skip and the adapter stays NOT VERIFIED.
const pgTestDSNEnv = "NODEVAULT_TEST_POSTGRES_DSN"

const liveTTL = time.Hour

// snapHealthy is an integrity_health snapshot value the tests pass through.
const snapHealthy = "Healthy"

// pgSchemaDSN creates a fresh schema and returns a DSN whose search_path points at it, so each
// test (and each helper process it spawns) gets its own empty store.
func pgSchemaDSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv(pgTestDSNEnv)
	if base == "" {
		t.Skipf("%s not set: PostgreSQL adapter test NOT VERIFIED", pgTestDSNEnv)
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	schema := fmt.Sprintf("nv_t_%d", time.Now().UnixNano())
	if _, err = db.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin, aerr := sql.Open("pgx", base)
		if aerr == nil {
			_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			_ = admin.Close()
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", pgTestDSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func openPG(t *testing.T, dsn string) *PostgresStore {
	t.Helper()
	s, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// rawExec runs SQL in the test schema directly, standing in for restore tooling or an operator.
func rawExec(t *testing.T, dsn, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func rawCount(t *testing.T, dsn, query string) int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err = db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func mustGet(t *testing.T, s *PostgresStore, buildID string) (Record, Fence) {
	t.Helper()
	rec, f, err := s.Get(context.Background(), buildID)
	if err != nil {
		t.Fatalf("Get(%q): %v", buildID, err)
	}
	return rec, f
}

func mustCreate(t *testing.T, s *PostgresStore, buildID, owner string) (Record, Fence) {
	t.Helper()
	rec, f, created, err := s.CreateOrGet(context.Background(), buildID, "sha256:spec", owner, liveTTL)
	if err != nil || !created {
		t.Fatalf("CreateOrGet(%q): created=%v err=%v", buildID, created, err)
	}
	return rec, f
}

// expire makes the owner's lease expire by the database clock.
func expire(t *testing.T, s *PostgresStore, buildID string, f Fence) {
	t.Helper()
	if _, err := s.RenewLease(context.Background(), buildID, f, time.Microsecond); err != nil {
		t.Fatalf("RenewLease(1µs): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
}

// TX1-07 (7): no DSN or an unreachable database is an error; there is no fallback store, and
// the DSN's credentials are not echoed.
func TestOpenPostgres_NoFallback(t *testing.T) {
	if _, err := OpenPostgres(context.Background(), "  "); err == nil {
		t.Fatal("empty DSN must be refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := OpenPostgres(ctx, "postgres://nv:s3cr3t-pw@127.0.0.1:1/nv?sslmode=disable&connect_timeout=2")
	if err == nil {
		_ = s.Close()
		t.Fatal("unreachable database must be refused, not replaced by another store")
	}
	if strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("error leaks the DSN password: %v", err)
	}
	if _, err := OpenPostgres(context.Background(), "postgres://nv:s3cr3t-pw@[::1"); err == nil ||
		strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("invalid DSN must be refused without echoing it: %v", err)
	}
}

// The J2 store refuses the SQLite store's unfenced restart recovery and writes nothing.
func TestPostgres_RecoverInterruptedUnfenced(t *testing.T) {
	dsn := pgSchemaDSN(t)
	s := openPG(t, dsn)
	_, f := mustCreate(t, s, "b1", "replica-a")
	if _, _, err := s.Transition(context.Background(), "b1", f, StatusBuilding, ""); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	before, beforeFence := mustGet(t, s, "b1")
	if n, err := s.RecoverInterrupted(time.Now()); !errors.Is(err, ErrUnfencedRecovery) || n != 0 {
		t.Fatalf("RecoverInterrupted = %d, %v; want 0, ErrUnfencedRecovery", n, err)
	}
	after, afterFence := mustGet(t, s, "b1")
	if after != before || afterFence != beforeFence {
		t.Fatalf("unfenced recovery wrote: %+v/%+v -> %+v/%+v", before, beforeFence, after, afterFence)
	}
}

// TX1-07 (2)/(6): BuildID→ToolSpecDigest is immutable and byte-exact; a lost commit
// acknowledgement retried by the same owner converges to the same record and fence; invalid
// identities write nothing.
func TestPostgres_CreateOrGetIdentity(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	rec, f := mustCreate(t, s, "b\x00x", "replica-a")
	if rec.Status != StatusRequested || f != (Fence{Owner: "replica-a", Generation: 1, Version: 1}) {
		t.Fatalf("created %+v fence %+v", rec, f)
	}

	again, againFence, created, err := s.CreateOrGet(ctx, "b\x00x", "sha256:spec", "replica-a", liveTTL)
	if err != nil || created || againFence != f || again != rec {
		t.Fatalf("owner retry must converge: %+v %+v created=%v err=%v", again, againFence, created, err)
	}
	_, other, created, err := s.CreateOrGet(ctx, "b\x00x", "sha256:spec", "replica-b", liveTTL)
	if err != nil || created || other != (Fence{}) {
		t.Fatalf("non-owner retry must get no fence: %+v created=%v err=%v", other, created, err)
	}
	for _, digest := range []string{"sha256:SPEC", "sha256:spec ", "sha256:spec\x00"} {
		if _, _, _, err := s.CreateOrGet(ctx, "b\x00x", digest, "replica-a", liveTTL); !errors.Is(err, ErrDigestConflict) {
			t.Fatalf("digest %q: want ErrDigestConflict, got %v", digest, err)
		}
	}
	if got, gotFence := mustGet(t, s, "b\x00x"); got != rec || gotFence != f {
		t.Fatalf("refused retries wrote: %+v %+v", got, gotFence)
	}
	// Byte-distinct IDs are distinct builds.
	mustCreate(t, s, "b\x00y", "replica-a")
	mustCreate(t, s, "B\x00x", "replica-a")

	for _, bad := range []struct{ id, digest, owner string }{
		{"", "sha256:spec", "replica-a"}, {"b2", "", "replica-a"}, {"b2", "sha256:spec", ""},
	} {
		if _, _, _, err := s.CreateOrGet(ctx, bad.id, bad.digest, bad.owner, liveTTL); err == nil {
			t.Fatalf("CreateOrGet(%q, %q, %q) must be refused", bad.id, bad.digest, bad.owner)
		}
	}
	if _, _, _, err := s.CreateOrGet(ctx, "b2", "sha256:spec", "replica-a", 0); err == nil {
		t.Fatal("zero lease TTL must be refused")
	}
	if n := rawCount(t, dsn, `SELECT count(*) FROM nv_build_state`); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}

// Fenced transitions bump the version; a replay of the committed transition converges; a stale
// version and an invalid status are refused without writing.
func TestPostgres_FencedTransition(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	_, f0 := mustCreate(t, s, "b1", "replica-a")

	rec, f1, err := s.Transition(ctx, "b1", f0, StatusBuilding, "")
	if err != nil || rec.Status != StatusBuilding || f1.Version != 2 {
		t.Fatalf("Transition: %+v %+v %v", rec, f1, err)
	}
	// Commit acknowledgement lost: the same call with the same fence converges.
	rec2, f2, err := s.Transition(ctx, "b1", f0, StatusBuilding, "")
	if err != nil || rec2 != rec || f2 != f1 {
		t.Fatalf("Transition replay: %+v %+v %v", rec2, f2, err)
	}
	if _, _, err = s.Transition(ctx, "b1", f0, StatusPushing, ""); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: want ErrVersionConflict, got %v", err)
	}
	if _, _, err = s.Transition(ctx, "b1", f1, Status("Bogus"), ""); err == nil {
		t.Fatal("invalid status must be refused")
	}
	if got, gotFence := mustGet(t, s, "b1"); got != rec || gotFence != f1 {
		t.Fatalf("refused writes changed the record: %+v %+v", got, gotFence)
	}
}

// Artifact references are fenced and immutable once recorded; the integrity_health snapshot may
// be refreshed.
func TestPostgres_FencedArtifactRefs(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	_, f1 := mustCreate(t, s, "b1", "replica-a")

	rec, f2, err := s.SetArtifact(ctx, "b1", f1, "harbor/x:1", "sha256:img")
	if err != nil || rec.ImageRef != "harbor/x:1" || f2.Version != 2 {
		t.Fatalf("SetArtifact: %+v %+v %v", rec, f2, err)
	}
	if _, f, err2 := s.SetArtifact(ctx, "b1", f1, "harbor/x:1", "sha256:img"); err2 != nil || f != f2 {
		t.Fatalf("SetArtifact replay: %+v %v", f, err2)
	}
	if _, f, err2 := s.SetArtifact(ctx, "b1", f2, "harbor/x:1", "sha256:img"); err2 != nil || f != f2 {
		t.Fatalf("SetArtifact same value: %+v %v", f, err2)
	}
	if _, _, err = s.SetArtifact(ctx, "b1", f2, "harbor/x:1", "sha256:other"); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("changed digest: want ErrArtifactConflict, got %v", err)
	}

	rec, f3, err := s.SetReferrer(ctx, "b1", f2, "sha256:ref", "Partial")
	if err != nil || rec.SpecReferrerDigest != "sha256:ref" || f3.Version != 3 {
		t.Fatalf("SetReferrer: %+v %+v %v", rec, f3, err)
	}
	rec, f4, err := s.SetReferrer(ctx, "b1", f3, "sha256:ref", snapHealthy)
	if err != nil || rec.IntegrityHealth != snapHealthy || f4.Version != 4 {
		t.Fatalf("SetReferrer snapshot refresh: %+v %+v %v", rec, f4, err)
	}
	if _, _, err = s.SetReferrer(ctx, "b1", f4, "sha256:other", snapHealthy); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("changed referrer: want ErrArtifactConflict, got %v", err)
	}
	if got, gotFence := mustGet(t, s, "b1"); got != rec || gotFence != f4 {
		t.Fatalf("refused writes changed the record: %+v %+v", got, gotFence)
	}
}

// A terminal transition converges on replay; later transitions get ErrAlreadyTerminal, the
// benign terminal race of the SQLite store.
func TestPostgres_FencedTerminal(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	_, f0 := mustCreate(t, s, "b1", "replica-a")

	rec, f1, err := s.Transition(ctx, "b1", f0, StatusSucceeded, "")
	if err != nil || rec.Status != StatusSucceeded {
		t.Fatalf("terminal Transition: %+v %v", rec, err)
	}
	if _, f, err2 := s.Transition(ctx, "b1", f0, StatusSucceeded, ""); err2 != nil || f != f1 {
		t.Fatalf("terminal replay must converge: %+v %v", f, err2)
	}
	if _, _, err = s.Transition(ctx, "b1", f1, StatusFailed, "late"); !errors.Is(err, ErrAlreadyTerminal) {
		t.Fatalf("after terminal: want ErrAlreadyTerminal, got %v", err)
	}
	if _, err = s.RenewLease(ctx, "b1", f1, liveTTL); !errors.Is(err, ErrAlreadyTerminal) {
		t.Fatalf("renew after terminal: want ErrAlreadyTerminal, got %v", err)
	}
	if got, gotFence := mustGet(t, s, "b1"); got != rec || gotFence != f1 {
		t.Fatalf("refused writes changed the record: %+v %+v", got, gotFence)
	}
}

// TX1-07 (4): after a takeover every fence of the old owner is refused and writes nothing; an
// expired lease refuses writes until renewed; a live lease cannot be reclaimed.
func TestPostgres_TakeoverRejectsStaleOwner(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	_, fa := mustCreate(t, s, "b1", "replica-a")
	_, fa, err := s.Transition(ctx, "b1", fa, StatusBuilding, "")
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if _, _, err = s.Reclaim(ctx, "b1", "replica-b", liveTTL); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("live lease: want ErrLeaseHeld, got %v", err)
	}

	expire(t, s, "b1", fa)
	if _, _, err = s.Transition(ctx, "b1", fa, StatusPushing, ""); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease: want ErrLeaseExpired, got %v", err)
	}
	recB, fb, err := s.Reclaim(ctx, "b1", "replica-b", liveTTL)
	if err != nil || fb != (Fence{Owner: "replica-b", Generation: 2, Version: fa.Version + 1}) || recB.Status != StatusBuilding {
		t.Fatalf("Reclaim: %+v %+v %v", recB, fb, err)
	}

	if _, _, err := s.Transition(ctx, "b1", fa, StatusPushing, ""); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old owner Transition: want ErrStaleFence, got %v", err)
	}
	if _, _, err := s.SetArtifact(ctx, "b1", fa, "harbor/x:1", "sha256:img"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old owner SetArtifact: want ErrStaleFence, got %v", err)
	}
	if _, _, err := s.SetReferrer(ctx, "b1", fa, "sha256:ref", snapHealthy); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old owner SetReferrer: want ErrStaleFence, got %v", err)
	}
	if _, err := s.RenewLease(ctx, "b1", fa, liveTTL); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old owner RenewLease: want ErrStaleFence, got %v", err)
	}
	stale := Fence{Owner: "replica-a", Generation: 1, Version: fb.Version}
	if _, _, err := s.Transition(ctx, "b1", stale, StatusPushing, ""); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("old generation at current version: want ErrStaleFence, got %v", err)
	}
	if got, gotFence := mustGet(t, s, "b1"); got != recB || gotFence != fb {
		t.Fatalf("stale writes changed the record: %+v %+v", got, gotFence)
	}
	if _, _, err := s.Transition(ctx, "b1", fb, StatusPushing, ""); err != nil {
		t.Fatalf("new owner Transition: %v", err)
	}
}

// An expired lease that nobody reclaimed can be renewed by its owner, which then writes again.
func TestPostgres_RenewExpiredBeforeReclaim(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	_, f := mustCreate(t, s, "b1", "replica-a")
	expire(t, s, "b1", f)
	f, err := s.RenewLease(ctx, "b1", f, liveTTL)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if _, _, err := s.Transition(ctx, "b1", f, StatusResolving, ""); err != nil {
		t.Fatalf("Transition after renew: %v", err)
	}
	if _, _, err := s.Reclaim(ctx, "b1", "replica-b", liveTTL); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("renewed lease: want ErrLeaseHeld, got %v", err)
	}
}

// A fence from before a later mutation cannot renew the lease: it is refused with
// ErrVersionConflict instead of being handed the newer version, and a stale heartbeat does not
// keep an expired lease from being reclaimed.
func TestPostgres_RenewRejectsStaleVersion(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s := openPG(t, dsn)
	_, f1 := mustCreate(t, s, "b1", "replica-a")
	rec, f2, err := s.Transition(ctx, "b1", f1, StatusBuilding, "")
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if f, err := s.RenewLease(ctx, "b1", f1, liveTTL); !errors.Is(err, ErrVersionConflict) || f != (Fence{}) {
		t.Fatalf("stale-version RenewLease = %+v, %v; want ErrVersionConflict", f, err)
	}
	expire(t, s, "b1", f2)
	if _, err := s.RenewLease(ctx, "b1", f1, liveTTL); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale-version heartbeat on expired lease: want ErrVersionConflict, got %v", err)
	}
	if got, gotFence := mustGet(t, s, "b1"); got != rec || gotFence != f2 {
		t.Fatalf("refused renew changed the record: %+v %+v", got, gotFence)
	}
	if _, _, err := s.Reclaim(ctx, "b1", "replica-b", liveTTL); err != nil {
		t.Fatalf("stale heartbeat must not keep the lease live: Reclaim: %v", err)
	}
}

// TX1-07 (3): closing and reopening keeps ID, digest, status, artifact refs, owner, generation
// and version, and the owner's fence keeps working.
func TestPostgres_ReopenPreserves(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	s, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, f := mustCreate(t, s, "b1", "replica-a")
	_, f, err = s.Transition(ctx, "b1", f, StatusPushing, "")
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	before, f, err := s.SetArtifact(ctx, "b1", f, "harbor/x:1", "sha256:img")
	if err != nil {
		t.Fatalf("SetArtifact: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r := openPG(t, dsn)
	after, afterFence := mustGet(t, r, "b1")
	if after != before || afterFence != f {
		t.Fatalf("reopen changed the record: %+v/%+v -> %+v/%+v", before, f, after, afterFence)
	}
	if _, _, err := r.Transition(ctx, "b1", f, StatusSucceeded, ""); err != nil {
		t.Fatalf("fence after reopen: %v", err)
	}
}

func requireHold(t *testing.T, s *PostgresStore, f Fence, what string) {
	t.Helper()
	ctx := context.Background()
	if _, _, _, err := s.CreateOrGet(ctx, "b-new", "sha256:spec", "replica-a", liveTTL); !errors.Is(err, ErrRestoreActivationHold) {
		t.Fatalf("%s: CreateOrGet must be refused with ErrRestoreActivationHold, got %v", what, err)
	}
	if _, _, err := s.Transition(ctx, "b1", f, StatusBuilding, ""); !errors.Is(err, ErrRestoreActivationHold) {
		t.Fatalf("%s: Transition must be refused with ErrRestoreActivationHold, got %v", what, err)
	}
	if _, err := s.RecoverExpired(ctx, "replica-b"); !errors.Is(err, ErrRestoreActivationHold) {
		t.Fatalf("%s: RecoverExpired must be refused with ErrRestoreActivationHold, got %v", what, err)
	}
}

// TX1-07 (8): a store whose activation belongs to another cluster/timeline/database refuses
// every mutation; reads keep working; raising the epoch or reopening does not lift the hold.
func TestPostgres_RestoreActivationHold(t *testing.T) {
	dsn := pgSchemaDSN(t)
	s := openPG(t, dsn)
	_, f := mustCreate(t, s, "b1", "replica-a")

	rawExec(t, dsn, `UPDATE nv_buildstate_activation SET timeline_id = timeline_id + 1`)
	requireHold(t, s, f, "identity mismatch")
	if rec, _ := mustGet(t, s, "b1"); rec.Status != StatusRequested {
		t.Fatalf("hold must write nothing: %+v", rec)
	}
	rawExec(t, dsn, `UPDATE nv_buildstate_activation SET epoch = epoch + 1`)
	requireHold(t, s, f, "epoch raised without evidence")
	_ = s.Close()
	requireHold(t, openPG(t, dsn), f, "reopen")
	if n := rawCount(t, dsn, `SELECT count(*) FROM nv_build_state`); n != 1 {
		t.Fatalf("hold must write nothing: %d rows", n)
	}
}

// A store holding builds without an activation record (e.g. a partial restore) opens held.
func TestPostgres_DataWithoutActivationIsHeld(t *testing.T) {
	dsn := pgSchemaDSN(t)
	s := openPG(t, dsn)
	_, f := mustCreate(t, s, "b1", "replica-a")
	_ = s.Close()
	rawExec(t, dsn, `DELETE FROM nv_buildstate_activation`)
	requireHold(t, openPG(t, dsn), f, "data without activation")
}

// TX1-07 (5): a foreign schema, a newer stamp or an unreadable stamp is refused before any
// table is created or row written.
func TestPostgres_SchemaOpenSafety(t *testing.T) {
	t.Run("foreign tables", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		rawExec(t, dsn, `CREATE TABLE nv_build_state (build_id text PRIMARY KEY, note text)`)
		rawExec(t, dsn, `INSERT INTO nv_build_state VALUES ('k', 'foreign row')`)
		if s, err := OpenPostgres(context.Background(), dsn); err == nil {
			_ = s.Close()
			t.Fatal("open over a foreign schema must be refused")
		}
		if n := rawCount(t, dsn, `SELECT count(*) FROM pg_tables WHERE tablename = 'nv_buildstate_schema_meta'
			AND schemaname = current_schema()`); n != 0 {
			t.Fatal("refused open must create nothing")
		}
		if n := rawCount(t, dsn, `SELECT count(*) FROM nv_build_state WHERE note = 'foreign row'`); n != 1 {
			t.Fatal("foreign row must be untouched")
		}
	})
	for _, stamp := range []string{"2", "1-invalid", "01", " 1", "0", ""} {
		t.Run("stamp "+stamp, func(t *testing.T) {
			dsn := pgSchemaDSN(t)
			s := openPG(t, dsn)
			mustCreate(t, s, "b1", "replica-a")
			_ = s.Close()
			rawExec(t, dsn, `UPDATE nv_buildstate_schema_meta SET value = $1 WHERE key = 'schema_version'`, stamp)
			rawExec(t, dsn, `DROP TABLE nv_buildstate_activation`)
			if s, err := OpenPostgres(context.Background(), dsn); err == nil {
				_ = s.Close()
				t.Fatalf("open with schema stamp %q must be refused", stamp)
			}
			if n := rawCount(t, dsn, `SELECT count(*) FROM pg_tables WHERE tablename = 'nv_buildstate_activation'
				AND schemaname = current_schema()`); n != 0 {
				t.Fatal("refused open must create nothing")
			}
		})
	}
	t.Run("meta without version row", func(t *testing.T) {
		dsn := pgSchemaDSN(t)
		s := openPG(t, dsn)
		mustCreate(t, s, "b1", "replica-a")
		_ = s.Close()
		rawExec(t, dsn, `DELETE FROM nv_buildstate_schema_meta`)
		if s, err := OpenPostgres(context.Background(), dsn); err == nil {
			_ = s.Close()
			t.Fatal("open without a schema_version row must be refused")
		}
		if n := rawCount(t, dsn, `SELECT count(*) FROM nv_buildstate_schema_meta`); n != 0 {
			t.Fatal("refused open must not re-stamp")
		}
	})
}
