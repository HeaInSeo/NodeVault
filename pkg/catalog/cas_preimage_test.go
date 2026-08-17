package catalog_test

import (
	"testing"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// baseTool returns a fresh tool with fixed content; callers vary only the
// registration-time / state fields to probe the casHash preimage.
func baseTool() *nfv1.RegisteredToolDefinition {
	return &nfv1.RegisteredToolDefinition{
		ToolName: "bwa-mem2",
		ImageUri: "registry.example.com/bwa-mem2:2.2.1",
		Digest:   "sha256:content-abc",
		Version:  "2.2.1",
	}
}

// ① Same content registered at different times → same casHash.
// RegisteredAt and Validation.LastValidatedAt must not enter the preimage.
func TestCasHash_ExcludesRegistrationTimestamps(t *testing.T) {
	cat := catalog.NewCatalogAt(t.TempDir())

	a := baseTool()
	a.RegisteredAt = 1000
	a.Validation = &nfv1.ValidationStatus{Phase: "Passed", LastValidatedAt: 1000}

	b := baseTool()
	b.RegisteredAt = 9999
	b.Validation = &nfv1.ValidationStatus{Phase: "Passed", LastValidatedAt: 9999}

	ha, err := cat.SaveWithCasHash(a)
	if err != nil {
		t.Fatalf("SaveWithCasHash a: %v", err)
	}
	hb, err := cat.SaveWithCasHash(b)
	if err != nil {
		t.Fatalf("SaveWithCasHash b: %v", err)
	}
	if ha != hb {
		t.Errorf("casHash changed with registration timestamps: %s vs %s", ha, hb)
	}
}

// ② Same content in different lifecycle_phase → same casHash (state axis 1).
func TestCasHash_ExcludesLifecyclePhase(t *testing.T) {
	cat := catalog.NewCatalogAt(t.TempDir())

	a := baseTool()
	a.LifecyclePhase = string(index.PhaseActive)
	b := baseTool()
	b.LifecyclePhase = string(index.PhaseRetracted)

	ha, err := cat.SaveWithCasHash(a)
	if err != nil {
		t.Fatalf("SaveWithCasHash a: %v", err)
	}
	hb, err := cat.SaveWithCasHash(b)
	if err != nil {
		t.Fatalf("SaveWithCasHash b: %v", err)
	}
	if ha != hb {
		t.Errorf("casHash changed with lifecycle_phase: %s vs %s", ha, hb)
	}
}

// state axis 2: same content in different integrity_health → same casHash.
func TestCasHash_ExcludesIntegrityHealth(t *testing.T) {
	cat := catalog.NewCatalogAt(t.TempDir())

	a := baseTool()
	a.IntegrityHealth = string(index.HealthPartial)
	b := baseTool()
	b.IntegrityHealth = string(index.HealthHealthy)

	ha, err := cat.SaveWithCasHash(a)
	if err != nil {
		t.Fatalf("SaveWithCasHash a: %v", err)
	}
	hb, err := cat.SaveWithCasHash(b)
	if err != nil {
		t.Fatalf("SaveWithCasHash b: %v", err)
	}
	if ha != hb {
		t.Errorf("casHash changed with integrity_health: %s vs %s", ha, hb)
	}
}

// Guard against over-exclusion: a genuine content change MUST change the hash.
func TestCasHash_ContentDifferenceChangesHash(t *testing.T) {
	cat := catalog.NewCatalogAt(t.TempDir())

	a := baseTool()
	b := baseTool()
	b.Digest = "sha256:content-XYZ" // real content difference

	ha, err := cat.SaveWithCasHash(a)
	if err != nil {
		t.Fatalf("SaveWithCasHash a: %v", err)
	}
	hb, err := cat.SaveWithCasHash(b)
	if err != nil {
		t.Fatalf("SaveWithCasHash b: %v", err)
	}
	if ha == hb {
		t.Errorf("distinct content produced identical casHash %s — over-excluded", ha)
	}
}

// Data variant: registration-time / state fields excluded; content change still
// changes the hash.
func TestCasHash_Data_ExcludesStateAndTimestamp(t *testing.T) {
	dc := catalog.NewDataCatalogAt(t.TempDir())

	base := func() *nfv1.RegisteredDataDefinition {
		return &nfv1.RegisteredDataDefinition{
			DataName:   "grch38",
			Version:    "p14",
			Checksum:   "sha256:data-abc",
			StorageUri: "oci://registry.example.com/grch38:p14",
		}
	}

	a := base()
	a.RegisteredAt = 1
	a.LifecyclePhase = string(index.PhaseActive)
	a.IntegrityHealth = string(index.HealthHealthy)
	b := base()
	b.RegisteredAt = 2
	b.LifecyclePhase = string(index.PhaseRetracted)
	b.IntegrityHealth = string(index.HealthPartial)

	ha, err := dc.SaveWithCasHash(a)
	if err != nil {
		t.Fatalf("SaveWithCasHash a: %v", err)
	}
	hb, err := dc.SaveWithCasHash(b)
	if err != nil {
		t.Fatalf("SaveWithCasHash b: %v", err)
	}
	if ha != hb {
		t.Errorf("data casHash changed with state/timestamp: %s vs %s", ha, hb)
	}

	c := base()
	c.Checksum = "sha256:data-DIFFERENT"
	hc, err := dc.SaveWithCasHash(c)
	if err != nil {
		t.Fatalf("SaveWithCasHash c: %v", err)
	}
	if hc == ha {
		t.Errorf("distinct data content produced identical casHash %s — over-excluded", ha)
	}
}

// §3 / §1.10: build-time RegisterTool records no fabricated validation.
func TestRegisterTool_NoFabricatedValidation(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "star",
		Version:  "2.7.11",
		Digest:   "sha256:star-abc",
		ImageUri: "registry.example.com/star:2.7.11",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if resp.Tool.Validation != nil {
		t.Errorf("build-time RegisterTool fabricated Validation %+v; want nil (unobserved)", resp.Tool.Validation)
	}
}

// §3-2 (가): idempotent re-registration of byte-identical content succeeds and
// returns the same casHash.
func TestRegisterTool_Idempotent_SameContentSameHash(t *testing.T) {
	svc := newTestService(t)

	req := &nfv1.RegisterToolRequest{
		ToolName: "salmon",
		Version:  "1.10.0",
		Digest:   "sha256:salmon-abc",
		ImageUri: "registry.example.com/salmon:1.10.0",
	}

	r1, err := svc.RegisterTool(t.Context(), req)
	if err != nil {
		t.Fatalf("RegisterTool #1: %v", err)
	}
	r2, err := svc.RegisterTool(t.Context(), req)
	if err != nil {
		t.Fatalf("RegisterTool #2 (re-registration) must be idempotent, got: %v", err)
	}
	if r1.CasHash != r2.CasHash {
		t.Errorf("re-registration changed casHash: %s vs %s", r1.CasHash, r2.CasHash)
	}
	if r2.Tool.LifecyclePhase != string(index.PhaseActive) {
		t.Errorf("re-registration of an Active tool: got %q want Active", r2.Tool.LifecyclePhase)
	}
}

// §3-2 Retracted case: re-registering a Retracted tool must surface Retracted,
// never a fabricated Active. Re-registration is not a lifecycle transition.
func TestRegisterTool_Idempotent_RetractedSurfacesRetracted(t *testing.T) {
	svc := newTestService(t)

	req := &nfv1.RegisterToolRequest{
		ToolName: "hisat2",
		Version:  "2.2.1",
		Digest:   "sha256:hisat2-abc",
		ImageUri: "registry.example.com/hisat2:2.2.1",
	}

	reg, err := svc.RegisterTool(t.Context(), req)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if _, retErr := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{
		CasHash: reg.CasHash,
		Reason:  "test retract",
	}); retErr != nil {
		t.Fatalf("RetractTool: %v", retErr)
	}

	re, err := svc.RegisterTool(t.Context(), req)
	if err != nil {
		t.Fatalf("re-registration after retract: %v", err)
	}
	if re.CasHash != reg.CasHash {
		t.Errorf("byte-identical re-registration changed casHash: %s vs %s", re.CasHash, reg.CasHash)
	}
	if re.Tool.LifecyclePhase != string(index.PhaseRetracted) {
		t.Errorf("re-registration of a Retracted tool returned %q; want Retracted (must not fabricate Active)", re.Tool.LifecyclePhase)
	}
}
