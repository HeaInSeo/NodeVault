package buildstate

import (
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
