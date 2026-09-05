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
