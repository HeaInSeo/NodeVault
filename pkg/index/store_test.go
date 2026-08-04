package index_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

const stableRefBWA1 = "bwa@1"

func newStore(t *testing.T) *index.Store {
	t.Helper()
	s, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	return s
}

func toolEntry(casHash, stableRef string) index.Entry {
	return index.Entry{
		CasHash:         casHash,
		ArtifactKind:    index.KindTool,
		StableRef:       stableRef,
		ToolName:        "bwa-mem2",
		Version:         "2.2.1",
		ImageDigest:     "sha256:aaaa",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}
}

// ── Append ────────────────────────────────────────────────────────────────────

func TestAppend_Success(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-001", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	all, _ := s.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(all))
	}
}

func TestAppend_EmptyCasHash_Rejected(t *testing.T) {
	s := newStore(t)
	err := s.Append(index.Entry{StableRef: stableRefBWA1})
	if err == nil {
		t.Fatal("expected error for empty CasHash")
	}
}

func TestAppend_DuplicateCasHash_Rejected(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-dup", stableRefBWA1)
	if err := s.Append(e); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if err := s.Append(e); err == nil {
		t.Fatal("expected error for duplicate CasHash")
	}
}

// ── GetByCasHash ──────────────────────────────────────────────────────────────

func TestGetByCasHash_Found(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-get", stableRefBWA1)
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.GetByCasHash("hash-get")
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if got.CasHash != "hash-get" {
		t.Errorf("CasHash: got %q want hash-get", got.CasHash)
	}
	if got.StableRef != stableRefBWA1 {
		t.Errorf("StableRef: got %q want %s", got.StableRef, stableRefBWA1)
	}
}

func TestGetByCasHash_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetByCasHash("nonexistent")
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── ListByStableRef ───────────────────────────────────────────────────────────

func TestListByStableRef_MultipleRevisions(t *testing.T) {
	s := newStore(t)
	// Two different casHashes share the same stableRef (1:N cardinality).
	for _, h := range []string{"hash-r1", "hash-r2"} {
		if err := s.Append(toolEntry(h, "bwa-mem2@2.2.1")); err != nil {
			t.Fatalf("Append %s: %v", h, err)
		}
	}
	// A third entry with a different stableRef.
	if err := s.Append(toolEntry("hash-other", "bowtie2@2.5.0")); err != nil {
		t.Fatalf("Append other: %v", err)
	}

	got, err := s.ListByStableRef("bwa-mem2@2.2.1")
	if err != nil {
		t.Fatalf("ListByStableRef: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries for bwa-mem2@2.2.1, got %d", len(got))
	}
}

func TestListByStableRef_NoMatch_EmptySlice(t *testing.T) {
	s := newStore(t)
	got, err := s.ListByStableRef("nonexistent@1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

// ── ListActive ────────────────────────────────────────────────────────────────

func TestListActive_LifecyclePhaseOnly(t *testing.T) {
	s := newStore(t)

	// Active + Healthy — should appear
	e1 := toolEntry("hash-a1", stableRefBWA1)
	e1.LifecyclePhase = index.PhaseActive
	e1.IntegrityHealth = index.HealthHealthy
	_ = s.Append(e1)

	// Active + Partial — should still appear (integrity_health irrelevant for Catalog)
	e2 := toolEntry("hash-a2", "bwa@2")
	e2.LifecyclePhase = index.PhaseActive
	e2.IntegrityHealth = index.HealthPartial
	_ = s.Append(e2)

	// Active + Missing — should still appear
	e3 := toolEntry("hash-a3", "bwa@3")
	e3.LifecyclePhase = index.PhaseActive
	e3.IntegrityHealth = index.HealthMissing
	_ = s.Append(e3)

	// Retracted — must NOT appear
	e4 := toolEntry("hash-r1", "bwa@4")
	e4.LifecyclePhase = index.PhaseRetracted
	_ = s.Append(e4)

	// Deleted — must NOT appear
	e5 := toolEntry("hash-d1", "bwa@5")
	e5.LifecyclePhase = index.PhaseDeleted
	_ = s.Append(e5)

	active, err := s.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 3 {
		t.Errorf("expected 3 Active entries, got %d", len(active))
	}
	for _, e := range active {
		if e.LifecyclePhase != index.PhaseActive {
			t.Errorf("non-Active entry in ListActive result: %q phase=%q", e.CasHash, e.LifecyclePhase)
		}
	}
}

// ── SetLifecyclePhase ─────────────────────────────────────────────────────────

func TestSetLifecyclePhase_Transition(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-lc", stableRefBWA1)
	e.LifecyclePhase = index.PhasePending
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := s.SetLifecyclePhase("hash-lc", index.PhaseActive); err != nil {
		t.Fatalf("SetLifecyclePhase: %v", err)
	}
	got, _ := s.GetByCasHash("hash-lc")
	if got.LifecyclePhase != index.PhaseActive {
		t.Errorf("LifecyclePhase: got %q want Active", got.LifecyclePhase)
	}
	if got.LifecycleUpdatedAt.IsZero() {
		t.Error("LifecycleUpdatedAt should be set")
	}
	// IntegrityHealth must be untouched by lifecycle transition
	if got.IntegrityHealth != index.HealthHealthy {
		t.Errorf("IntegrityHealth should be unchanged: got %q", got.IntegrityHealth)
	}
}

func TestSetLifecyclePhase_NotFound(t *testing.T) {
	s := newStore(t)
	err := s.SetLifecyclePhase("nonexistent", index.PhaseActive)
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// appendAtPhase appends a tool entry starting at the given lifecycle_phase.
func appendAtPhase(t *testing.T, s *index.Store, casHash string, phase index.LifecyclePhase) {
	t.Helper()
	e := toolEntry(casHash, stableRefBWA1)
	e.LifecyclePhase = phase
	if err := s.Append(e); err != nil {
		t.Fatalf("Append(%s): %v", phase, err)
	}
}

// TestSetLifecyclePhase_AllowedEdges verifies each of the four §4.4 edges
// succeeds: Pending→Active, Active→Retracted, Retracted→Active, Retracted→Deleted.
func TestSetLifecyclePhase_AllowedEdges(t *testing.T) {
	cases := []struct {
		name string
		from index.LifecyclePhase
		to   index.LifecyclePhase
	}{
		{"Pending->Active", index.PhasePending, index.PhaseActive},
		{"Active->Retracted", index.PhaseActive, index.PhaseRetracted},
		{"Retracted->Active", index.PhaseRetracted, index.PhaseActive},
		{"Retracted->Deleted", index.PhaseRetracted, index.PhaseDeleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			appendAtPhase(t, s, "hash-edge", tc.from)
			if err := s.SetLifecyclePhase("hash-edge", tc.to); err != nil {
				t.Fatalf("SetLifecyclePhase %s: unexpected error %v", tc.name, err)
			}
			got, _ := s.GetByCasHash("hash-edge")
			if got.LifecyclePhase != tc.to {
				t.Errorf("LifecyclePhase: got %q want %q", got.LifecyclePhase, tc.to)
			}
		})
	}
}

// TestSetLifecyclePhase_ActiveToDeleted_Rejected is the F-3 regression test:
// §4.4 forbids Active → Deleted (Retracted must be traversed first). Before the
// transition table was enforced, SetLifecyclePhase overwrote phase blindly and
// this transition silently succeeded.
func TestSetLifecyclePhase_ActiveToDeleted_Rejected(t *testing.T) {
	s := newStore(t)
	appendAtPhase(t, s, "hash-ad", index.PhaseActive)

	err := s.SetLifecyclePhase("hash-ad", index.PhaseDeleted)
	if !errors.Is(err, index.ErrInvalidLifecycleTransition) {
		t.Fatalf("expected ErrInvalidLifecycleTransition, got %v", err)
	}
	got, _ := s.GetByCasHash("hash-ad")
	if got.LifecyclePhase != index.PhaseActive {
		t.Errorf("rejected transition must leave phase Active, got %q", got.LifecyclePhase)
	}
}

// TestSetLifecyclePhase_ForbiddenEdges_Rejected covers the remaining unlisted
// edges: Pending→Retracted, every Deleted→* edge (Deleted is terminal), and
// self-edges (rejected per this change; §4.4 lists no self-edge).
func TestSetLifecyclePhase_ForbiddenEdges_Rejected(t *testing.T) {
	cases := []struct {
		name string
		from index.LifecyclePhase
		to   index.LifecyclePhase
	}{
		{"Pending->Retracted", index.PhasePending, index.PhaseRetracted},
		{"Pending->Deleted", index.PhasePending, index.PhaseDeleted},
		{"Deleted->Active", index.PhaseDeleted, index.PhaseActive},
		{"Deleted->Retracted", index.PhaseDeleted, index.PhaseRetracted},
		{"Deleted->Pending", index.PhaseDeleted, index.PhasePending},
		{"Active->Active", index.PhaseActive, index.PhaseActive},
		{"Retracted->Retracted", index.PhaseRetracted, index.PhaseRetracted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			appendAtPhase(t, s, "hash-fb", tc.from)
			before, _ := s.GetByCasHash("hash-fb")

			err := s.SetLifecyclePhase("hash-fb", tc.to)
			if !errors.Is(err, index.ErrInvalidLifecycleTransition) {
				t.Fatalf("%s: expected ErrInvalidLifecycleTransition, got %v", tc.name, err)
			}
			// Rejection must not mutate the entry.
			got, _ := s.GetByCasHash("hash-fb")
			if got.LifecyclePhase != tc.from {
				t.Errorf("%s: rejected transition changed phase to %q, want %q", tc.name, got.LifecyclePhase, tc.from)
			}
			if !got.LifecycleUpdatedAt.Equal(before.LifecycleUpdatedAt) {
				t.Errorf("%s: rejected transition changed LifecycleUpdatedAt %v -> %v", tc.name, before.LifecycleUpdatedAt, got.LifecycleUpdatedAt)
			}
		})
	}
}

// ── SetIntegrityHealth ────────────────────────────────────────────────────────

func TestSetIntegrityHealth_Transition(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-ih", stableRefBWA1)
	e.IntegrityHealth = index.HealthHealthy
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := s.SetIntegrityHealth("hash-ih", index.HealthPartial); err != nil {
		t.Fatalf("SetIntegrityHealth: %v", err)
	}
	got, _ := s.GetByCasHash("hash-ih")
	if got.IntegrityHealth != index.HealthPartial {
		t.Errorf("IntegrityHealth: got %q want Partial", got.IntegrityHealth)
	}
	if got.HealthCheckedAt.IsZero() {
		t.Error("HealthCheckedAt should be set")
	}
	// LifecyclePhase must be untouched by health transition
	if got.LifecyclePhase != index.PhaseActive {
		t.Errorf("LifecyclePhase should be unchanged: got %q", got.LifecyclePhase)
	}
}

func TestSetIntegrityHealth_NotFound(t *testing.T) {
	s := newStore(t)
	err := s.SetIntegrityHealth("nonexistent", index.HealthMissing)
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── Two-axis independence ─────────────────────────────────────────────────────

// TestTwoAxesAreIndependent verifies that SetLifecyclePhase and
// SetIntegrityHealth each change only their own axis, never the other.
func TestTwoAxesAreIndependent(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-2ax", stableRefBWA1)
	e.LifecyclePhase = index.PhasePending
	e.IntegrityHealth = index.HealthHealthy
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Transition lifecycle: Pending → Active; health must stay Healthy
	if err := s.SetLifecyclePhase("hash-2ax", index.PhaseActive); err != nil {
		t.Fatalf("SetLifecyclePhase: %v", err)
	}
	got, _ := s.GetByCasHash("hash-2ax")
	if got.IntegrityHealth != index.HealthHealthy {
		t.Errorf("after lifecycle transition, IntegrityHealth changed: %q", got.IntegrityHealth)
	}

	// Transition health: Healthy → Missing; lifecycle must stay Active
	if err := s.SetIntegrityHealth("hash-2ax", index.HealthMissing); err != nil {
		t.Fatalf("SetIntegrityHealth: %v", err)
	}
	got, _ = s.GetByCasHash("hash-2ax")
	if got.LifecyclePhase != index.PhaseActive {
		t.Errorf("after health transition, LifecyclePhase changed: %q", got.LifecyclePhase)
	}
}

// ── Persistence ───────────────────────────────────────────────────────────────

func TestPersistence_ReloadFromDisk(t *testing.T) {
	dir := t.TempDir()

	s1, _ := index.NewAt(dir)
	e := toolEntry("hash-persist", stableRefBWA1)
	if err := s1.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Open a second Store pointing at the same dir — simulates restart.
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt (reload): %v", err)
	}
	got, err := s2.GetByCasHash("hash-persist")
	if err != nil {
		t.Fatalf("GetByCasHash after reload: %v", err)
	}
	if got.StableRef != stableRefBWA1 {
		t.Errorf("StableRef after reload: got %q want %s", got.StableRef, stableRefBWA1)
	}
}

// ── Data artifact support (P3 reservation) ───────────────────────────────────

func TestAppend_DataKind_AcceptedBySchema(t *testing.T) {
	s := newStore(t)
	e := index.Entry{
		CasHash:         "hash-data",
		ArtifactKind:    index.KindData,
		StableRef:       "grch38-genome@2024",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}
	if err := s.Append(e); err != nil {
		t.Fatalf("Append data artifact: %v", err)
	}
	got, err := s.GetByCasHash("hash-data")
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if got.ArtifactKind != index.KindData {
		t.Errorf("ArtifactKind: got %q want data", got.ArtifactKind)
	}
}

// ── ResolvedToolSpec ──────────────────────────────────────────────────────────

func TestAppendResolvedToolSpec_Success(t *testing.T) {
	s := newStore(t)
	r := index.ResolvedToolSpec{
		ToolSpecDigest: "spec-001",
		ToolName:       "bwa-mem2",
		Version:        "2.2.1",
		RawSpec:        `{"tool_name":"bwa-mem2"}`,
	}
	if err := s.AppendResolvedToolSpec(r); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}
	got, err := s.GetResolvedToolSpecByDigest("spec-001")
	if err != nil {
		t.Fatalf("GetResolvedToolSpecByDigest: %v", err)
	}
	if got.ToolName != "bwa-mem2" {
		t.Errorf("ToolName: got %q want bwa-mem2", got.ToolName)
	}
	if got.ResolvedAt.IsZero() {
		t.Error("ResolvedAt should be auto-populated")
	}
}

func TestAppendResolvedToolSpec_EmptyDigest_Rejected(t *testing.T) {
	s := newStore(t)
	if err := s.AppendResolvedToolSpec(index.ResolvedToolSpec{ToolName: "bwa"}); err == nil {
		t.Fatal("expected error for empty ToolSpecDigest")
	}
}

func TestAppendResolvedToolSpec_Duplicate_Rejected(t *testing.T) {
	s := newStore(t)
	r := index.ResolvedToolSpec{ToolSpecDigest: "spec-dup"}
	if err := s.AppendResolvedToolSpec(r); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := s.AppendResolvedToolSpec(r); err == nil {
		t.Fatal("expected error for duplicate ToolSpecDigest")
	}
}

func TestUpsertResolvedToolSpec_Duplicate_ReturnsExisting(t *testing.T) {
	s := newStore(t)
	original := index.ResolvedToolSpec{
		ToolSpecDigest: "spec-upsert",
		ToolName:       "bwa-mem2",
		Version:        "2.2.1",
		ResolvedAt:     time.Unix(100, 0).UTC(),
	}
	if _, err := s.UpsertResolvedToolSpec(original); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	replacement := index.ResolvedToolSpec{
		ToolSpecDigest: "spec-upsert",
		ToolName:       "changed",
		Version:        "changed",
		ResolvedAt:     time.Unix(200, 0).UTC(),
	}
	got, err := s.UpsertResolvedToolSpec(replacement)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got.ToolName != original.ToolName || got.Version != original.Version {
		t.Fatalf("got %q@%q, want original %q@%q", got.ToolName, got.Version, original.ToolName, original.Version)
	}
	if !got.ResolvedAt.Equal(original.ResolvedAt) {
		t.Fatalf("ResolvedAt got %s, want original %s", got.ResolvedAt, original.ResolvedAt)
	}
}

func TestUpsertResolvedToolSpec_EmptyDigest_Rejected(t *testing.T) {
	s := newStore(t)
	if _, err := s.UpsertResolvedToolSpec(index.ResolvedToolSpec{ToolName: "bwa"}); err == nil {
		t.Fatal("expected error for empty ToolSpecDigest")
	}
}

func TestGetResolvedToolSpecByDigest_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetResolvedToolSpecByDigest("nonexistent")
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListResolvedToolSpecs_ReturnsAll(t *testing.T) {
	s := newStore(t)
	_ = s.AppendResolvedToolSpec(index.ResolvedToolSpec{ToolSpecDigest: "spec-a"})
	_ = s.AppendResolvedToolSpec(index.ResolvedToolSpec{ToolSpecDigest: "spec-b"})
	got, err := s.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}
}

// ── Schema v1 → v2 backward-compatible load ───────────────────────────────────

// TestLoad_SchemaV1File_NoNewSections_LoadsEmptySlices verifies that a vault-index.json
// written before schema v2 (missing the resolved_tool_specs field) loads cleanly with
// that section as empty, not nil-panicking.
func TestLoad_SchemaV1File_NoNewSections_LoadsEmptySlices(t *testing.T) {
	dir := t.TempDir()
	v1JSON := `{"schema_version":1,"entries":[{"cas_hash":"hash-v1","artifact_kind":"tool","stable_ref":"bwa@1","lifecycle_phase":"Active","integrity_health":"Healthy"}]}`
	if err := os.WriteFile(dir+"/vault-index.json", []byte(v1JSON), 0o600); err != nil {
		t.Fatalf("write v1 fixture: %v", err)
	}

	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	// Old entry still loads.
	got, err := s.GetByCasHash("hash-v1")
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if got.StableRef != stableRefBWA1 {
		t.Errorf("StableRef: got %q want %s", got.StableRef, stableRefBWA1)
	}

	// New section is empty, not erroring.
	specs, err := s.ListResolvedToolSpecs()
	if err != nil || len(specs) != 0 {
		t.Errorf("ListResolvedToolSpecs: got %v, err %v", specs, err)
	}

	// New record can be appended on top of the migrated v1 file.
	if err := s.AppendResolvedToolSpec(index.ResolvedToolSpec{ToolSpecDigest: "spec-after-migration"}); err != nil {
		t.Errorf("AppendResolvedToolSpec after v1 load: %v", err)
	}
}

// ── ToolBuildRecord ───────────────────────────────────────────────────────────

func TestAppendToolBuildRecord_Success(t *testing.T) {
	s := newStore(t)
	r := index.ToolBuildRecord{
		BuildID:        "build-001",
		ToolSpecDigest: "spec-001",
		ImageDigest:    "sha256:bbbb",
		Backend:        "in-pod-buildah",
		Success:        true,
	}
	if err := s.AppendToolBuildRecord(r); err != nil {
		t.Fatalf("AppendToolBuildRecord: %v", err)
	}
	got, err := s.GetToolBuildRecordByBuildID("build-001")
	if err != nil {
		t.Fatalf("GetToolBuildRecordByBuildID: %v", err)
	}
	if !got.Success {
		t.Error("Success: got false want true")
	}
	if got.Backend != "in-pod-buildah" {
		t.Errorf("Backend: got %q want in-pod-buildah", got.Backend)
	}
}

func TestAppendToolBuildRecord_EmptyBuildID_Rejected(t *testing.T) {
	s := newStore(t)
	if err := s.AppendToolBuildRecord(index.ToolBuildRecord{}); err == nil {
		t.Fatal("expected error for empty BuildID")
	}
}

func TestAppendToolBuildRecord_Duplicate_Rejected(t *testing.T) {
	s := newStore(t)
	r := index.ToolBuildRecord{BuildID: "build-dup"}
	if err := s.AppendToolBuildRecord(r); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := s.AppendToolBuildRecord(r); err == nil {
		t.Fatal("expected error for duplicate BuildID")
	}
}

func TestGetToolBuildRecordByBuildID_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetToolBuildRecordByBuildID("nonexistent")
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListToolBuildRecordsByToolSpecDigest_MultipleBuilds(t *testing.T) {
	s := newStore(t)
	_ = s.AppendToolBuildRecord(index.ToolBuildRecord{BuildID: "b1", ToolSpecDigest: "spec-x"})
	_ = s.AppendToolBuildRecord(index.ToolBuildRecord{BuildID: "b2", ToolSpecDigest: "spec-x"})
	_ = s.AppendToolBuildRecord(index.ToolBuildRecord{BuildID: "b3", ToolSpecDigest: "spec-y"})

	got, err := s.ListToolBuildRecordsByToolSpecDigest("spec-x")
	if err != nil {
		t.Fatalf("ListToolBuildRecordsByToolSpecDigest: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 records, got %d", len(got))
	}
}

func TestListToolBuildRecordsByToolSpecDigest_NoMatch_EmptySlice(t *testing.T) {
	s := newStore(t)
	got, err := s.ListToolBuildRecordsByToolSpecDigest("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

// ── ToolImageRecord ───────────────────────────────────────────────────────────

func TestAppendToolImageRecord_Success(t *testing.T) {
	s := newStore(t)
	r := index.ToolImageRecord{
		ImageDigest:    "sha256:cccc",
		ImageRef:       "harbor.example.com/library/bwa:latest",
		ToolSpecDigest: "spec-001",
		BuildID:        "build-001",
		Platform:       "linux/amd64",
	}
	if err := s.AppendToolImageRecord(r); err != nil {
		t.Fatalf("AppendToolImageRecord: %v", err)
	}
	got, err := s.GetToolImageRecordByDigest("sha256:cccc")
	if err != nil {
		t.Fatalf("GetToolImageRecordByDigest: %v", err)
	}
	if got.BuildID != "build-001" {
		t.Errorf("BuildID: got %q want build-001", got.BuildID)
	}
	if got.PushedAt.IsZero() {
		t.Error("PushedAt should be auto-populated")
	}
}

func TestAppendToolImageRecord_EmptyDigest_Rejected(t *testing.T) {
	s := newStore(t)
	if err := s.AppendToolImageRecord(index.ToolImageRecord{}); err == nil {
		t.Fatal("expected error for empty ImageDigest")
	}
}

func TestAppendToolImageRecord_Duplicate_Rejected(t *testing.T) {
	s := newStore(t)
	r := index.ToolImageRecord{ImageDigest: "sha256:dup"}
	if err := s.AppendToolImageRecord(r); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := s.AppendToolImageRecord(r); err == nil {
		t.Fatal("expected error for duplicate (ImageDigest, BuildID)")
	}
}

// TestAppendToolImageRecord_SameDigestDifferentBuildID_BothRecorded guards a
// reproducibility regression: a repeat build that reproduces the exact same
// digest is itself evidence of reproducibility and must get its own record
// instead of being silently dropped because some other BuildID already
// claimed that ImageDigest.
func TestAppendToolImageRecord_SameDigestDifferentBuildID_BothRecorded(t *testing.T) {
	s := newStore(t)
	first := index.ToolImageRecord{ImageDigest: "sha256:repro", BuildID: "build-a"}
	second := index.ToolImageRecord{ImageDigest: "sha256:repro", BuildID: "build-b"}
	if err := s.AppendToolImageRecord(first); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := s.AppendToolImageRecord(second); err != nil {
		t.Fatalf("second append (same digest, different build_id): %v", err)
	}

	gotA, err := s.ListToolImageRecordsByBuildID("build-a")
	if err != nil || len(gotA) != 1 {
		t.Fatalf("ListToolImageRecordsByBuildID(build-a) = %v, %v", gotA, err)
	}
	gotB, err := s.ListToolImageRecordsByBuildID("build-b")
	if err != nil || len(gotB) != 1 {
		t.Fatalf("ListToolImageRecordsByBuildID(build-b) = %v, %v", gotB, err)
	}
}

// TestGetLatestToolImageRecordByRef_ReturnsMostRecent guards the #27 tag-
// reassignment-detection path: when the same ImageRef (e.g. a reused
// version tag) has multiple records — a rebuild moved it to a new digest —
// the most recently pushed one must win.
func TestGetLatestToolImageRecordByRef_ReturnsMostRecent(t *testing.T) {
	s := newStore(t)
	const ref = "harbor.example.com/library/bwa:0.7.17"
	older := index.ToolImageRecord{ImageDigest: "sha256:older", ImageRef: ref, BuildID: "build-a", PushedAt: time.Unix(100, 0).UTC()}
	newer := index.ToolImageRecord{ImageDigest: "sha256:newer", ImageRef: ref, BuildID: "build-b", PushedAt: time.Unix(200, 0).UTC()}
	if err := s.AppendToolImageRecord(older); err != nil {
		t.Fatalf("append older: %v", err)
	}
	if err := s.AppendToolImageRecord(newer); err != nil {
		t.Fatalf("append newer: %v", err)
	}

	got, err := s.GetLatestToolImageRecordByRef(ref)
	if err != nil {
		t.Fatalf("GetLatestToolImageRecordByRef: %v", err)
	}
	if got.ImageDigest != "sha256:newer" {
		t.Errorf("ImageDigest = %q, want sha256:newer (the more recently pushed record)", got.ImageDigest)
	}
}

func TestGetLatestToolImageRecordByRef_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetLatestToolImageRecordByRef("no/such:ref")
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGetToolImageRecordByDigest_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetToolImageRecordByDigest("nonexistent")
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListToolImageRecordsByBuildID_MultiplePlatforms(t *testing.T) {
	s := newStore(t)
	_ = s.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:p1", BuildID: "build-multi", Platform: "linux/amd64"})
	_ = s.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:p2", BuildID: "build-multi", Platform: "linux/arm64"})

	got, err := s.ListToolImageRecordsByBuildID("build-multi")
	if err != nil {
		t.Fatalf("ListToolImageRecordsByBuildID: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 records, got %d", len(got))
	}
}

func TestCertifiedToolImageRecord_KeysByToolSpecDigestAndPlatform(t *testing.T) {
	s := newStore(t)
	amd64 := index.CertifiedToolImageRecord{
		ImageDigest: "sha256:amd64", ToolSpecDigest: "spec-001", Platform: "linux/amd64",
	}
	arm64 := index.CertifiedToolImageRecord{
		ImageDigest: "sha256:arm64", ToolSpecDigest: "spec-001", Platform: "linux/arm64",
	}
	if err := s.UpsertCertifiedToolImageRecord(amd64); err != nil {
		t.Fatalf("Upsert amd64: %v", err)
	}
	if err := s.UpsertCertifiedToolImageRecord(arm64); err != nil {
		t.Fatalf("Upsert arm64: %v", err)
	}
	got, err := s.GetCertifiedToolImageRecordByToolSpecDigestAndPlatform("spec-001", "linux/arm64")
	if err != nil {
		t.Fatalf("Get by composite key: %v", err)
	}
	if got.ImageDigest != "sha256:arm64" {
		t.Errorf("ImageDigest: got %q want sha256:arm64", got.ImageDigest)
	}
}

// TestLoad_SchemaV1File_ToolBuildAndImageSections_LoadEmptySlices verifies that a
// vault-index.json written before schema v2 (missing tool_build_records and
// tool_image_records fields) loads cleanly with those sections as empty, and
// that new ToolBuildRecord/ToolImageRecord entries can be appended afterward.
func TestLoad_SchemaV1File_ToolBuildAndImageSections_LoadEmptySlices(t *testing.T) {
	dir := t.TempDir()
	v1JSON := `{"schema_version":1,"entries":[{"cas_hash":"hash-v1b","artifact_kind":"tool","stable_ref":"bwa@1","lifecycle_phase":"Active","integrity_health":"Healthy"}]}`
	if err := os.WriteFile(dir+"/vault-index.json", []byte(v1JSON), 0o600); err != nil {
		t.Fatalf("write v1 fixture: %v", err)
	}

	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	builds, err := s.ListToolBuildRecordsByToolSpecDigest("anything")
	if err != nil || len(builds) != 0 {
		t.Errorf("ListToolBuildRecordsByToolSpecDigest: got %v, err %v", builds, err)
	}

	if err := s.AppendToolBuildRecord(index.ToolBuildRecord{BuildID: "build-after-migration"}); err != nil {
		t.Errorf("AppendToolBuildRecord after v1 load: %v", err)
	}
	if err := s.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:after-migration"}); err != nil {
		t.Errorf("AppendToolImageRecord after v1 load: %v", err)
	}
}

// TestLoad_SchemaV3File_ValidationRequestSection_LoadsEmptySlice verifies
// that a vault-index.json written before schema v4 (missing
// validation_request_records) loads cleanly with that section empty, and
// that a new ValidationRequestRecord can be created afterward. Regression
// guard for the PR2-A schema bump — mirrors
// TestLoad_SchemaV1File_ToolBuildAndImageSections_LoadEmptySlices above.
func TestLoad_SchemaV3File_ValidationRequestSection_LoadsEmptySlice(t *testing.T) {
	dir := t.TempDir()
	v3JSON := `{"schema_version":3,"entries":[{"cas_hash":"hash-v3","artifact_kind":"tool","stable_ref":"bwa@1","lifecycle_phase":"Active","integrity_health":"Healthy"}]}`
	if err := os.WriteFile(dir+"/vault-index.json", []byte(v3JSON), 0o600); err != nil {
		t.Fatalf("write v3 fixture: %v", err)
	}

	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	if _, err := s.GetValidationRequestRecord("anything"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("GetValidationRequestRecord on a pre-v4 file: err = %v, want ErrNotFound", err)
	}

	if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{
		ValidationRequestID: "vr-after-migration",
	}); err != nil {
		t.Errorf("CreateValidationRequestRecord after v3 load: %v", err)
	}
}

// TestNewIndex_StampsCurrentSchemaVersion is a regression guard for a bug an
// independent review caught: schema.go's indexFile doc comment claimed
// "schema_version >= 4: ... ValidationRequestRecords" while the schemaVersion
// constant in this file was left at 3, so every freshly created index would
// have been stamped with a version number one behind what the file's own
// content actually required. Nothing currently branches on the stamped
// number, so this produced no functional bug yet — but it would silently
// misroute any future version-gated migration. This asserts the two stay in
// sync going forward by checking the number actually written to disk, not
// just the in-memory struct default.
func TestNewIndex_StampsCurrentSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	// NewAt alone does not write vault-index.json — load() only populates the
	// in-memory struct for a not-yet-existing file; the file is only created
	// on the first write. Force that write so there's something on disk to
	// inspect the stamped schema_version of.
	if err = s.CreateValidationRequestRecord(index.ValidationRequestRecord{ValidationRequestID: "vr-stamp-check"}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}

	data, err := os.ReadFile(dir + "/vault-index.json") //nolint:gosec // G304: dir is t.TempDir(), not user input.
	if err != nil {
		t.Fatalf("read vault-index.json: %v", err)
	}
	var stamped struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &stamped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	const wantSchemaVersion = 4 // bump alongside indexFile's doc comment in schema.go
	if stamped.SchemaVersion != wantSchemaVersion {
		t.Errorf("stamped schema_version = %d, want %d", stamped.SchemaVersion, wantSchemaVersion)
	}
}

// ── ValidationRequestRecord ───────────────────────────────────────────────────

func TestCreateValidationRequestRecord_Success(t *testing.T) {
	s := newStore(t)

	if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{
		ValidationRequestID: "vr-1",
		BuildID:             "build-1",
		CasHash:             "hash-1",
		ImageDigest:         "sha256:aaaa",
	}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}

	got, err := s.GetValidationRequestRecord("vr-1")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if got.ValidationStatus != index.ValidationEnqueuePending {
		t.Errorf("ValidationStatus = %q, want %q (default)", got.ValidationStatus, index.ValidationEnqueuePending)
	}
	if got.RequestedAt.IsZero() {
		t.Error("RequestedAt was not defaulted")
	}
	if got.BuildID != "build-1" {
		t.Errorf("BuildID = %q, want build-1", got.BuildID)
	}
}

func TestCreateValidationRequestRecord_EmptyID_Rejected(t *testing.T) {
	s := newStore(t)
	err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{})
	if err == nil {
		t.Fatal("expected an error for empty ValidationRequestID")
	}
}

func TestCreateValidationRequestRecord_DuplicateID_Rejected(t *testing.T) {
	s := newStore(t)
	rec := index.ValidationRequestRecord{ValidationRequestID: "vr-dup"}
	if err := s.CreateValidationRequestRecord(rec); err != nil {
		t.Fatalf("first CreateValidationRequestRecord: %v", err)
	}
	if err := s.CreateValidationRequestRecord(rec); err == nil {
		t.Fatal("expected an error for a duplicate ValidationRequestID")
	}
}

func TestGetValidationRequestRecord_NotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetValidationRequestRecord("missing")
	if !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestTransitionValidationRequest_EnqueuePendingToQueued_AppliesMutateAndStatus
// exercises the success path CreateValidationRequestRecord's caller
// actually uses: EnqueuePending -> Queued with SentinelJobID filled in.
func TestTransitionValidationRequest_EnqueuePendingToQueued_AppliesMutateAndStatus(t *testing.T) {
	s := newStore(t)
	if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{ValidationRequestID: "vr-1"}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}

	err := s.TransitionValidationRequest("vr-1", index.ValidationQueued, func(r *index.ValidationRequestRecord) {
		r.SentinelJobID = "job-123"
	})
	if err != nil {
		t.Fatalf("TransitionValidationRequest: %v", err)
	}

	got, err := s.GetValidationRequestRecord("vr-1")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if got.ValidationStatus != index.ValidationQueued {
		t.Errorf("ValidationStatus = %q, want Queued", got.ValidationStatus)
	}
	if got.SentinelJobID != "job-123" {
		t.Errorf("SentinelJobID = %q, want job-123", got.SentinelJobID)
	}
}

// TestTransitionValidationRequest_InvalidEdge_Rejected is a direct
// regression guard for the corruption scenario flagged in review: applying
// an out-of-order or stale status update (here, a terminal Succeeded record
// being pushed back to Running) must be rejected, not silently accepted.
func TestTransitionValidationRequest_InvalidEdge_Rejected(t *testing.T) {
	tests := []struct {
		name string
		from index.ValidationStatus
		to   index.ValidationStatus
	}{
		{"succeeded cannot go back to running", index.ValidationSucceeded, index.ValidationRunning},
		{"failed cannot go back to queued", index.ValidationFailed, index.ValidationQueued},
		{"enqueue_pending cannot skip straight to succeeded", index.ValidationEnqueuePending, index.ValidationSucceeded},
		{"queued cannot go back to enqueue_pending", index.ValidationQueued, index.ValidationEnqueuePending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{
				ValidationRequestID: "vr-1",
				ValidationStatus:    tt.from,
			}); err != nil {
				t.Fatalf("CreateValidationRequestRecord: %v", err)
			}

			err := s.TransitionValidationRequest("vr-1", tt.to, nil)
			if err == nil {
				t.Fatalf("expected an error transitioning %s -> %s, got nil", tt.from, tt.to)
			}

			got, getErr := s.GetValidationRequestRecord("vr-1")
			if getErr != nil {
				t.Fatalf("GetValidationRequestRecord: %v", getErr)
			}
			if got.ValidationStatus != tt.from {
				t.Errorf("ValidationStatus after rejected transition = %q, want unchanged %q", got.ValidationStatus, tt.from)
			}
		})
	}
}

// TestTransitionValidationRequest_EnqueuePendingToRunning_Allowed guards the
// fix for a real race: NodeSentinel can execute a job and submit a result
// before NodeVault's own postBuildRegistration has processed the enqueue
// response and driven the record to Queued. Without this edge, a result
// arriving that early would leave the record orphaned at EnqueuePending
// forever — see applyValidationCorrelationLocked.
func TestTransitionValidationRequest_EnqueuePendingToRunning_Allowed(t *testing.T) {
	s := newStore(t)
	if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{ValidationRequestID: "vr-1"}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}
	// Default status is EnqueuePending (see CreateValidationRequestRecord).
	if err := s.TransitionValidationRequest("vr-1", index.ValidationRunning, nil); err != nil {
		t.Fatalf("TransitionValidationRequest EnqueuePending -> Running: %v", err)
	}
	got, err := s.GetValidationRequestRecord("vr-1")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if got.ValidationStatus != index.ValidationRunning {
		t.Errorf("ValidationStatus = %q, want Running", got.ValidationStatus)
	}
}

// TestTransitionValidationRequest_LateEnqueueAck_DoesNotRegressFromRunning
// is the other half of the same race guard: once a fast result has already
// promoted the record past EnqueuePending, a late-arriving enqueue ACK
// attempting its own Queued transition must be rejected, not regress a
// record that has already moved forward.
func TestTransitionValidationRequest_LateEnqueueAck_DoesNotRegressFromRunning(t *testing.T) {
	s := newStore(t)
	if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{ValidationRequestID: "vr-1"}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}
	if err := s.TransitionValidationRequest("vr-1", index.ValidationRunning, nil); err != nil {
		t.Fatalf("TransitionValidationRequest EnqueuePending -> Running: %v", err)
	}

	// Simulate the late enqueue ACK's own Queued transition attempt.
	err := s.TransitionValidationRequest("vr-1", index.ValidationQueued, func(r *index.ValidationRequestRecord) {
		r.SentinelJobID = "job-from-late-ack"
	})
	if err == nil {
		t.Fatal("expected the late ACK's Running -> Queued transition to be rejected")
	}

	got, getErr := s.GetValidationRequestRecord("vr-1")
	if getErr != nil {
		t.Fatalf("GetValidationRequestRecord: %v", getErr)
	}
	if got.ValidationStatus != index.ValidationRunning {
		t.Errorf("ValidationStatus = %q, want unchanged Running (must not regress to Queued)", got.ValidationStatus)
	}
}

// TestTransitionValidationRequest_RejectedEdge_WrapsErrInvalidTransition
// locks in the sentinel error contract callers rely on to distinguish an
// expected, already-progressed race (see pkg/build's postBuildRegistration,
// which logs this at Info rather than Warn) from a genuine storage failure.
func TestTransitionValidationRequest_RejectedEdge_WrapsErrInvalidTransition(t *testing.T) {
	s := newStore(t)
	if err := s.CreateValidationRequestRecord(index.ValidationRequestRecord{
		ValidationRequestID: "vr-1", ValidationStatus: index.ValidationRunning,
	}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}

	err := s.TransitionValidationRequest("vr-1", index.ValidationQueued, nil)
	if !errors.Is(err, index.ErrInvalidTransition) {
		t.Fatalf("err = %v, want it to wrap ErrInvalidTransition", err)
	}
}

func TestTransitionValidationRequest_NotFound(t *testing.T) {
	s := newStore(t)
	err := s.TransitionValidationRequest("missing", index.ValidationQueued, nil)
	if !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
