package index

import (
	"errors"
	"testing"
)

func tfDurRec(cas, tfd, img string) RegisteredToolFunction {
	return RegisteredToolFunction{
		CasHash:             cas,
		ToolFunctionDigest:  tfd,
		FunctionImageDigest: img,
		ArtifactKind:        KindToolFunction,
		LifecyclePhase:      PhaseActive,
		IntegrityHealth:     HealthPartial,
	}
}

// TestRegisterToolFunctionAtomic_PostRenameDurabilityDoesNotDivergeMemory is the
// regression for the P1 post-rename durability finding: if save() fails AFTER os.Rename
// has already swapped the new index onto disk, the in-memory mutation must NOT be rolled
// back. Rolling it back would diverge memory from disk, and a later unrelated save() would
// then overwrite the committed file with the stale in-memory index, permanently deleting
// the record. Before the fix, the second registration would be lost after the third save.
func TestRegisterToolFunctionAtomic_PostRenameDurabilityDoesNotDivergeMemory(t *testing.T) {
	dir := t.TempDir()
	s, err := NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	// A: a normal, fully-durable registration.
	if _, _, err = s.RegisterToolFunctionAtomic("req-a", tfDurRec("cas-a", "tfd-a", "img-a"), nil); err != nil {
		t.Fatalf("register A: %v", err)
	}

	// B: injected post-rename durability failure. The rename succeeds (disk gets B), then
	// the simulated parent-dir fsync fails, so save() returns errIndexPersistedNotDurable.
	failAfterRenameForTest = func() error { return errors.New("simulated post-rename dir fsync failure") }
	t.Cleanup(func() { failAfterRenameForTest = nil })
	_, _, err = s.RegisterToolFunctionAtomic("req-b", tfDurRec("cas-b", "tfd-b", "img-b"), nil)
	if err == nil {
		t.Fatal("expected a durability error for the post-rename failure")
	}
	if !errors.Is(err, errIndexPersistedNotDurable) {
		t.Fatalf("post-rename failure must be tagged errIndexPersistedNotDurable, got %v", err)
	}
	// B must remain in memory (it is already on disk); memory must not diverge from disk.
	if _, e := s.GetToolFunctionByCasHash("cas-b"); e != nil {
		t.Fatalf("B was rolled out of memory after a post-rename failure (memory/disk divergence): %v", e)
	}

	// C: a later successful save. With the bug, memory would be {A,C} (B rolled back) and
	// this save would delete B from disk. With the fix, memory is {A,B,C}.
	failAfterRenameForTest = nil
	if _, _, err = s.RegisterToolFunctionAtomic("req-c", tfDurRec("cas-c", "tfd-c", "img-c"), nil); err != nil {
		t.Fatalf("register C: %v", err)
	}

	// Reload from disk: all three must survive. The post-rename record (B) must not have
	// been erased by the later save.
	s2, err := NewAt(dir)
	if err != nil {
		t.Fatalf("reload NewAt: %v", err)
	}
	for _, cas := range []string{"cas-a", "cas-b", "cas-c"} {
		if _, e := s2.GetToolFunctionByCasHash(cas); e != nil {
			t.Fatalf("after reload, %s is missing — a committed record was lost: %v", cas, e)
		}
	}
}

// TestRegisterToolFunctionAtomic_ReplayRepairsUncertainDurability is the regression for the
// round P1 durability-replay gap: after a first registration whose rename succeeded but
// parent-dir fsync failed (durability uncertain), a subsequent idempotent replay must NOT
// acknowledge success without first re-saving (re-fsyncing) the index. It re-saves on the
// next call and only acks once durability is confirmed.
func TestRegisterToolFunctionAtomic_ReplayRepairsUncertainDurability(t *testing.T) {
	dir := t.TempDir()
	s, err := NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	rec := tfDurRec("cas-1", "tfd-1", "img-1")

	// First call: rename succeeds, dir fsync fails → uncertain durability.
	failAfterRenameForTest = func() error { return errors.New("simulated post-rename dir fsync failure") }
	t.Cleanup(func() { failAfterRenameForTest = nil })
	if _, _, err = s.RegisterToolFunctionAtomic("req-1", rec, nil); !errors.Is(err, errIndexPersistedNotDurable) {
		t.Fatalf("first call: want errIndexPersistedNotDurable, got %v", err)
	}
	if !s.toolFunctionDurabilityUncertain {
		t.Fatal("durability-uncertain flag not set after post-rename failure")
	}

	// Replay while the fault persists: the top-of-function re-save fails again, so the replay
	// must NOT acknowledge success, and the flag stays set (no durability gap acknowledged).
	if _, _, err = s.RegisterToolFunctionAtomic("req-1", rec, nil); err == nil {
		t.Fatal("replay acknowledged success while durability was still uncertain")
	}
	if !s.toolFunctionDurabilityUncertain {
		t.Fatal("flag cleared despite a still-failing re-save")
	}

	// Clear the fault and replay: the re-save now fsyncs the dir, durability is confirmed,
	// the flag clears, and the idempotent replay returns success.
	failAfterRenameForTest = nil
	stored, created, rerr := s.RegisterToolFunctionAtomic("req-1", rec, nil)
	if rerr != nil {
		t.Fatalf("replay after repair: %v", rerr)
	}
	if created {
		t.Fatal("idempotent replay must not create a new record")
	}
	if s.toolFunctionDurabilityUncertain {
		t.Fatal("flag not cleared after a successful re-save")
	}
	if stored.CasHash != "cas-1" {
		t.Fatalf("replay returned wrong record: %+v", stored)
	}
	// Durable on reload.
	s2, err := NewAt(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, e := s2.GetToolFunctionByCasHash("cas-1"); e != nil {
		t.Fatalf("record not durable after repair: %v", e)
	}
}

// TestStore_StartupDirSyncInvokedAndFailClosed is the regression for the round-2 P1
// durability-across-restart gap: the Store must conservatively fsync the index parent
// directory at open/load (so a visible-but-un-fsync'd rename from before a crash becomes
// durable before the Store serves), and must fail closed if that sync fails.
func TestStore_StartupDirSyncInvokedAndFailClosed(t *testing.T) {
	dir := t.TempDir()

	// (1) startup dir-sync is invoked on open.
	var calls int
	syncDirForTest = func(string) error { calls++; return nil }
	t.Cleanup(func() { syncDirForTest = nil })
	if _, err := NewAt(dir); err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if calls == 0 {
		t.Fatal("startup directory fsync was not invoked at Store open")
	}

	// (2) startup dir-sync failure fails closed: no usable Store is returned.
	syncDirForTest = func(string) error { return errors.New("simulated startup dir-sync failure") }
	s, err := NewAt(dir)
	if err == nil {
		t.Fatal("Store open must fail closed when startup dir-sync fails")
	}
	if s != nil {
		t.Fatal("no Store may be returned when startup durability cannot be established")
	}
}

// TestRegisterToolFunctionAtomic_ReplaySafeAcrossRestart proves a post-rename-uncertain
// registration's record is present and idempotently replayable after a simulated process
// restart (a fresh Store over the same dir, whose startup fsync assures durability before it
// serves). The in-memory uncertainty flag is intentionally not carried across the restart;
// startup durability covers it.
func TestRegisterToolFunctionAtomic_ReplaySafeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	rec := tfDurRec("cas-1", "tfd-1", "img-1")

	// First registration: rename succeeds, dir fsync fails → uncertain, error (not acked).
	failAfterRenameForTest = func() error { return errors.New("post-rename dir fsync failure") }
	t.Cleanup(func() { failAfterRenameForTest = nil })
	if _, _, e := s.RegisterToolFunctionAtomic("req-1", rec, nil); !errors.Is(e, errIndexPersistedNotDurable) {
		t.Fatalf("first call: want errIndexPersistedNotDurable, got %v", e)
	}

	// Simulate process restart: fault cleared, fresh Store over the same dir (startup fsync).
	failAfterRenameForTest = nil
	s2, err := NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, e := s2.GetToolFunctionByCasHash("cas-1"); e != nil {
		t.Fatalf("record not present after restart: %v", e)
	}
	stored, created, rerr := s2.RegisterToolFunctionAtomic("req-1", rec, nil)
	if rerr != nil {
		t.Fatalf("replay after restart: %v", rerr)
	}
	if created {
		t.Fatal("idempotent replay after restart must not create a new record")
	}
	if stored.CasHash != "cas-1" {
		t.Fatalf("replay returned wrong record: %+v", stored)
	}
}

// TestAppendToolImageRecord_PostRenameDurabilityKeepsRecord mirrors the post-rename
// durability contract for the ToolImageRecord path (the digest->locator mapping the W3
// function base resolution relies on): a failure AFTER os.Rename already wrote the record
// to disk must NOT roll the in-memory append back, or memory would diverge from disk and a
// later save() would drop the committed record.
func TestAppendToolImageRecord_PostRenameDurabilityKeepsRecord(t *testing.T) {
	dir := t.TempDir()
	s, err := NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	failAfterRenameForTest = func() error { return errors.New("simulated post-rename dir fsync failure") }
	t.Cleanup(func() { failAfterRenameForTest = nil })
	err = s.AppendToolImageRecord(ToolImageRecord{ImageDigest: "sha256:img", ImageRef: "reg/repo:tag", BuildID: "b"})
	if err == nil {
		t.Fatal("expected a durability error for the post-rename failure")
	}
	if !errors.Is(err, errIndexPersistedNotDurable) {
		t.Fatalf("post-rename failure must be tagged errIndexPersistedNotDurable, got %v", err)
	}
	failAfterRenameForTest = nil
	if _, e := s.GetLatestToolImageRecordByDigest("sha256:img"); e != nil {
		t.Fatalf("record rolled out of memory after a post-rename failure (memory/disk divergence): %v", e)
	}
}
