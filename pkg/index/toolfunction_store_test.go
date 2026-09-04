package index_test

import (
	"errors"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

const (
	casA    = "casA"
	casB    = "casB"
	tfd1    = "tfd1"
	imgA    = "imgA"
	imgB    = "imgB"
	revX    = "revX"
	reqID1  = "req-1"
	missing = "nope"
)

func tfRecord(casHash, toolFunctionDigest, imageDigest string) index.RegisteredToolFunction {
	return index.RegisteredToolFunction{
		CasHash:             casHash,
		ToolFunctionDigest:  toolFunctionDigest,
		FunctionImageDigest: imageDigest,
		ArtifactKind:        index.KindToolFunction,
		LifecyclePhase:      index.PhaseActive,
		IntegrityHealth:     index.HealthPartial,
	}
}

func TestRegisterToolFunctionAtomic_NewActive(t *testing.T) {
	s := newStore(t)
	stored, created, err := s.RegisterToolFunctionAtomic(reqID1, tfRecord(casA, tfd1, imgA), nil)
	if err != nil || !created {
		t.Fatalf("new registration: created=%v err=%v", created, err)
	}
	if stored.LifecyclePhase != index.PhaseActive {
		t.Fatalf("phase=%q want Active", stored.LifecyclePhase)
	}
	if stored.RegisteredAt.IsZero() {
		t.Fatal("RegisteredAt not stamped")
	}
	got, err := s.GetToolFunctionByCasHash(casA)
	if err != nil || got.ToolFunctionDigest != tfd1 {
		t.Fatalf("readback: %+v err=%v", got, err)
	}
}

func TestRegisterToolFunctionAtomic_EmptyCasHash(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.RegisterToolFunctionAtomic("r", tfRecord("", "tfd", "img"), nil); err == nil {
		t.Fatal("expected error for empty CasHash")
	}
}

func TestRegisterToolFunctionAtomic_ContentIdempotent(t *testing.T) {
	s := newStore(t)
	first, _, err := s.RegisterToolFunctionAtomic(reqID1, tfRecord(casA, tfd1, imgA), nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same content, same request id → idempotent replay, no new record, phase preserved.
	second, created, err := s.RegisterToolFunctionAtomic(reqID1, tfRecord(casA, tfd1, imgA), nil)
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	if !second.RegisteredAt.Equal(first.RegisteredAt) {
		t.Fatal("idempotent replay must return the existing record verbatim (RegisteredAt changed)")
	}
}

func TestRegisterToolFunctionAtomic_RequestIDConflict(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.RegisterToolFunctionAtomic(reqID1, tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same request id, different resulting cas hash → conflict, no mutation.
	_, _, err := s.RegisterToolFunctionAtomic(reqID1, tfRecord(casB, tfd1, imgB), nil)
	if !errors.Is(err, index.ErrToolFunctionRequestConflict) {
		t.Fatalf("expected ErrToolFunctionRequestConflict, got %v", err)
	}
	if _, gerr := s.GetToolFunctionByCasHash(casB); !errors.Is(gerr, index.ErrNotFound) {
		t.Fatal("conflicting registration must not persist a record")
	}
}

func TestRegisterToolFunctionAtomic_ContentIdempotentAcrossRequestIDs(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.RegisterToolFunctionAtomic("reqA", tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("reqA: %v", err)
	}
	// Different request id, same content → returns existing record, records the mapping.
	out, created, err := s.RegisterToolFunctionAtomic("reqB", tfRecord(casA, tfd1, imgA), nil)
	if err != nil || created {
		t.Fatalf("reqB: created=%v err=%v", created, err)
	}
	if out.CasHash != casA {
		t.Fatalf("out cas=%q want casA", out.CasHash)
	}
	for _, id := range []string{"reqA", "reqB"} {
		rec, gerr := s.GetToolFunctionRequestRecord(id)
		if gerr != nil || rec.CasHash != casA {
			t.Fatalf("request record %q: %+v err=%v", id, rec, gerr)
		}
	}
}

func TestRegisterToolFunctionAtomic_MultiplicityInvariant(t *testing.T) {
	s := newStore(t)
	// Same tool_function_digest, different function image → distinct cas records coexist.
	if _, created, err := s.RegisterToolFunctionAtomic("r1", tfRecord(casA, tfd1, imgA), nil); err != nil || !created {
		t.Fatalf("r1: created=%v err=%v", created, err)
	}
	if _, created, err := s.RegisterToolFunctionAtomic("r2", tfRecord(casB, tfd1, imgB), nil); err != nil || !created {
		t.Fatalf("r2: created=%v err=%v", created, err)
	}
	a, errA := s.GetToolFunctionByCasHash(casA)
	b, errB := s.GetToolFunctionByCasHash(casB)
	if errA != nil || errB != nil {
		t.Fatalf("both must coexist: errA=%v errB=%v", errA, errB)
	}
	if a.ToolFunctionDigest != b.ToolFunctionDigest {
		t.Fatal("same tool_function_digest expected for both")
	}
	if a.CasHash == b.CasHash {
		t.Fatal("distinct images must have distinct cas hashes")
	}
}

func TestRegisterToolFunctionAtomic_PresentationRevision(t *testing.T) {
	s := newStore(t)
	rev := &index.ToolFunctionPresentationRevision{
		RevisionID:       revX,
		CasHash:          casA,
		PresentationJSON: `{"label":"X"}`,
	}
	if _, _, err := s.RegisterToolFunctionAtomic("r1", tfRecordWithRev(casA, tfd1, imgA, revX), rev); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := s.GetToolFunctionPresentationRevision(revX)
	if err != nil || got.PresentationJSON != `{"label":"X"}` {
		t.Fatalf("revision readback: %+v err=%v", got, err)
	}
	// A second record sharing the same revision id does not duplicate the revision.
	sameRev := &index.ToolFunctionPresentationRevision{RevisionID: revX, CasHash: casB, PresentationJSON: `{"label":"X"}`}
	if _, _, rerr := s.RegisterToolFunctionAtomic("r2", tfRecordWithRev(casB, tfd1, imgB, revX), sameRev); rerr != nil {
		t.Fatalf("register 2: %v", rerr)
	}
	got2, err := s.GetToolFunctionPresentationRevision(revX)
	if err != nil || got2.CasHash != casA {
		t.Fatalf("shared revision must keep the first association: %+v err=%v", got2, err)
	}
}

func TestToolFunctionReads_NotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.GetToolFunctionByCasHash(missing); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("cas not found: %v", err)
	}
	if _, err := s.GetToolFunctionPresentationRevision(missing); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("revision not found: %v", err)
	}
	if _, err := s.GetToolFunctionRequestRecord(missing); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("request record not found: %v", err)
	}
}

// TestRegisterToolFunctionAtomic_PersistAndReload proves atomic durability + schema
// v5 round-trip: a record survives a fresh Store opened on the same directory.
func TestRegisterToolFunctionAtomic_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s1, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	rev := &index.ToolFunctionPresentationRevision{RevisionID: revX, CasHash: casA, PresentationJSON: `{"label":"X"}`}
	if _, _, rerr := s1.RegisterToolFunctionAtomic(reqID1, tfRecordWithRev(casA, tfd1, imgA, revX), rev); rerr != nil {
		t.Fatalf("register: %v", rerr)
	}
	// Reopen from disk.
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.GetToolFunctionByCasHash(casA); err != nil {
		t.Fatalf("record lost across reload: %v", err)
	}
	if _, err := s2.GetToolFunctionPresentationRevision(revX); err != nil {
		t.Fatalf("revision lost across reload: %v", err)
	}
	if _, err := s2.GetToolFunctionRequestRecord(reqID1); err != nil {
		t.Fatalf("request record lost across reload: %v", err)
	}
}

func tfRecordWithRev(casHash, toolFunctionDigest, imageDigest, revID string) index.RegisteredToolFunction {
	r := tfRecord(casHash, toolFunctionDigest, imageDigest)
	r.PresentationRevisionID = revID
	return r
}
