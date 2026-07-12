package buildstate

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "build-state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return store
}

func TestBuildState_CreateAndGet(t *testing.T) {
	store := newStore(t)
	now := time.Unix(100, 0).UTC()

	created, err := store.Create("build-1", "spec-1", now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Status != StatusRequested {
		t.Fatalf("Status got %q want Requested", created.Status)
	}

	got, err := store.Get("build-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BuildID != "build-1" || got.ToolSpecDigest != "spec-1" || got.Status != StatusRequested {
		t.Fatalf("unexpected record: %+v", got)
	}
	if !got.RequestedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected timestamps: %+v", got)
	}
}

func TestBuildStateStore_SetArtifact_PersistsImageRefDigest(t *testing.T) {
	store := newStore(t)
	now := time.Unix(100, 0).UTC()
	if _, err := store.Create("build-artifact", "spec-1", now); err != nil {
		t.Fatalf("Create: %v", err)
	}

	later := time.Unix(200, 0).UTC()
	updated, err := store.SetArtifact("build-artifact", "harbor.example.com/library/tool:latest", "sha256:deadbeef", later)
	if err != nil {
		t.Fatalf("SetArtifact: %v", err)
	}
	if updated.ImageRef != "harbor.example.com/library/tool:latest" || updated.ImageDigest != "sha256:deadbeef" {
		t.Fatalf("unexpected record after SetArtifact: %+v", updated)
	}
	if !updated.UpdatedAt.Equal(later) {
		t.Fatalf("UpdatedAt not bumped: got %v want %v", updated.UpdatedAt, later)
	}

	got, err := store.Get("build-artifact")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ImageRef != "harbor.example.com/library/tool:latest" || got.ImageDigest != "sha256:deadbeef" {
		t.Fatalf("SetArtifact did not persist: %+v", got)
	}
}

func TestBuildStateStore_SetArtifact_NotFound(t *testing.T) {
	store := newStore(t)
	if _, err := store.SetArtifact("missing-build", "ref", "digest", time.Unix(100, 0).UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestBuildStateStore_SetReferrer_PersistsReferrerAndIntegrityHealth(t *testing.T) {
	store := newStore(t)
	now := time.Unix(100, 0).UTC()
	if _, err := store.Create("build-referrer", "spec-1", now); err != nil {
		t.Fatalf("Create: %v", err)
	}

	later := time.Unix(200, 0).UTC()
	updated, err := store.SetReferrer("build-referrer", "sha256:referrerdigest", "Healthy", later)
	if err != nil {
		t.Fatalf("SetReferrer: %v", err)
	}
	if updated.SpecReferrerDigest != "sha256:referrerdigest" || updated.IntegrityHealth != "Healthy" {
		t.Fatalf("unexpected record after SetReferrer: %+v", updated)
	}

	got, err := store.Get("build-referrer")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SpecReferrerDigest != "sha256:referrerdigest" || got.IntegrityHealth != "Healthy" {
		t.Fatalf("SetReferrer did not persist: %+v", got)
	}
}

// TestBuildStateStore_EnsureColumn_MigratesExistingDB verifies that opening a
// database created before Sprint 7 (only the original six columns) adds the
// new artifact columns in place, without losing existing rows.
func TestBuildStateStore_EnsureColumn_MigratesExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacyDB, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	ctx := context.Background()
	if _, execErr := legacyDB.ExecContext(ctx, `
CREATE TABLE build_state (
	build_id TEXT PRIMARY KEY,
	tool_spec_digest TEXT NOT NULL,
	status TEXT NOT NULL,
	failure_reason TEXT NOT NULL DEFAULT '',
	requested_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
)`); execErr != nil {
		t.Fatalf("create legacy table: %v", execErr)
	}
	if _, execErr := legacyDB.ExecContext(ctx,
		`INSERT INTO build_state (build_id, tool_spec_digest, status, failure_reason, requested_at, updated_at)
VALUES (?, ?, ?, '', ?, ?)`,
		"legacy-build", "legacy-spec", string(StatusSucceeded), int64(100000), int64(100000),
	); execErr != nil {
		t.Fatalf("insert legacy row: %v", execErr)
	}
	if closeErr := legacyDB.Close(); closeErr != nil {
		t.Fatalf("close legacy db: %v", closeErr)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy schema: %v", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Fatalf("Close: %v", closeErr)
		}
	}()

	got, err := store.Get("legacy-build")
	if err != nil {
		t.Fatalf("Get after migration: %v", err)
	}
	if got.ToolSpecDigest != "legacy-spec" || got.Status != StatusSucceeded {
		t.Fatalf("legacy row not preserved: %+v", got)
	}
	if got.ImageRef != "" || got.ImageDigest != "" || got.SpecReferrerDigest != "" || got.IntegrityHealth != "" {
		t.Fatalf("expected new columns to default to empty string, got %+v", got)
	}

	updated, err := store.SetArtifact("legacy-build", "harbor.example.com/library/tool:latest", "sha256:abc", time.Unix(200, 0).UTC())
	if err != nil {
		t.Fatalf("SetArtifact after migration: %v", err)
	}
	if updated.ImageDigest != "sha256:abc" {
		t.Fatalf("ImageDigest after SetArtifact: got %q", updated.ImageDigest)
	}
}

func TestBuildState_RecoverInterrupted(t *testing.T) {
	store := newStore(t)
	now := time.Unix(100, 0).UTC()
	for _, id := range []string{"requested", "resolving", "building", "pushing"} {
		if _, err := store.Create(id, "spec-"+id, now); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if _, err := store.Transition("resolving", StatusResolving, "", now.Add(time.Second)); err != nil {
		t.Fatalf("Transition resolving: %v", err)
	}
	if _, err := store.Transition("building", StatusBuilding, "", now.Add(time.Second)); err != nil {
		t.Fatalf("Transition building: %v", err)
	}
	if _, err := store.Transition("pushing", StatusPushing, "", now.Add(time.Second)); err != nil {
		t.Fatalf("Transition pushing: %v", err)
	}

	recovered, err := store.RecoverInterrupted(now.Add(2 * time.Second))
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 4 {
		t.Fatalf("recovered got %d want 4", recovered)
	}
	for _, id := range []string{"requested", "resolving", "building", "pushing"} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.Status != StatusInterrupted {
			t.Fatalf("%s status got %q want Interrupted", id, got.Status)
		}
	}
}

func TestBuildState_NeverAutoSucceedInterrupted(t *testing.T) {
	store := newStore(t)
	now := time.Unix(100, 0).UTC()
	if _, err := store.Create("build-1", "spec-1", now); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.RecoverInterrupted(now.Add(time.Second)); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	_, err := store.Transition("build-1", StatusSucceeded, "", now.Add(2*time.Second))
	if err == nil {
		t.Fatal("expected terminal Interrupted build not to transition to Succeeded")
	}
	got, err := store.Get("build-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInterrupted {
		t.Fatalf("status got %q want Interrupted", got.Status)
	}
}

func TestBuildState_RecoverInterrupted_LeavesTerminalRecords(t *testing.T) {
	store := newStore(t)
	now := time.Unix(100, 0).UTC()
	for _, id := range []string{"succeeded", "failed"} {
		if _, err := store.Create(id, "spec-"+id, now); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if _, err := store.Transition("succeeded", StatusSucceeded, "", now.Add(time.Second)); err != nil {
		t.Fatalf("Transition succeeded: %v", err)
	}
	if _, err := store.Transition("failed", StatusFailed, "boom", now.Add(time.Second)); err != nil {
		t.Fatalf("Transition failed: %v", err)
	}

	recovered, err := store.RecoverInterrupted(now.Add(2 * time.Second))
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered got %d want 0", recovered)
	}
	for id, want := range map[string]Status{"succeeded": StatusSucceeded, "failed": StatusFailed} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.Status != want {
			t.Fatalf("%s status got %q want %q", id, got.Status, want)
		}
	}
}

func TestBuildState_GetNotFound(t *testing.T) {
	store := newStore(t)
	_, err := store.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
