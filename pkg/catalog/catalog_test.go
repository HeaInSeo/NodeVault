package catalog_test

import (
	"os"
	"path/filepath"
	"testing"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

func newTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	return catalog.NewCatalogAt(t.TempDir())
}

func newTestService(t *testing.T) *catalog.ToolRegistryService {
	t.Helper()
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return catalog.NewToolRegistryService(cat, store)
}

// TestSave_SameContent_SameHash verifies that identical content produces the same CAS key.
func TestSave_SameContent_SameHash(t *testing.T) {
	cat := newTestCatalog(t)

	tool := &nfv1.RegisteredToolDefinition{
		ToolName: "bwa-mem2",
		ImageUri: "registry.example.com/bwa-mem2:2.2.1",
		Digest:   "sha256:abc123",
	}

	hash1, err := cat.Save(tool)
	if err != nil {
		t.Fatalf("Save #1: %v", err)
	}

	hash2, err := cat.Save(tool)
	if err != nil {
		t.Fatalf("Save #2: %v", err)
	}

	if hash1 != hash2 {
		t.Errorf("same content produced different hashes: %s vs %s", hash1, hash2)
	}
}

// TestSave_DifferentContent_DifferentHash verifies distinct content produces distinct hashes.
func TestSave_DifferentContent_DifferentHash(t *testing.T) {
	cat := newTestCatalog(t)

	tool1 := &nfv1.RegisteredToolDefinition{ToolName: "tool-a", Digest: "sha256:aaa"}
	tool2 := &nfv1.RegisteredToolDefinition{ToolName: "tool-b", Digest: "sha256:bbb"}

	hash1, err := cat.Save(tool1)
	if err != nil {
		t.Fatalf("Save tool1: %v", err)
	}
	hash2, err := cat.Save(tool2)
	if err != nil {
		t.Fatalf("Save tool2: %v", err)
	}

	if hash1 == hash2 {
		t.Error("different content produced the same hash")
	}
}

// TestSave_FileExists verifies that a .tooldefinition file is written to disk.
func TestSave_FileExists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CATALOG_DIR", dir)
	cat := catalog.NewCatalog()

	tool := &nfv1.RegisteredToolDefinition{ToolName: "samtools", Digest: "sha256:def456"}
	hash, err := cat.Save(tool)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(dir, hash+".tooldefinition")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("expected file %s to exist", path)
	}
}

// TestList_ReturnsAllSaved verifies that List returns all saved tools.
func TestList_ReturnsAllSaved(t *testing.T) {
	cat := newTestCatalog(t)

	tools := []*nfv1.RegisteredToolDefinition{
		{ToolName: "alpha", Digest: "sha256:001"},
		{ToolName: "beta", Digest: "sha256:002"},
		{ToolName: "gamma", Digest: "sha256:003"},
	}
	for _, tool := range tools {
		if _, err := cat.Save(tool); err != nil {
			t.Fatalf("Save %s: %v", tool.ToolName, err)
		}
	}

	listed, err := cat.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != len(tools) {
		t.Errorf("List returned %d tools, want %d", len(listed), len(tools))
	}
}

// TestRegisterTool_CasHashPopulated verifies RegisterTool sets CasHash on the returned tool.
func TestRegisterTool_CasHashPopulated(t *testing.T) {
	svc := newTestService(t)

	req := &nfv1.RegisterToolRequest{
		RequestId:        "req-001",
		ToolDefinitionId: "def-001",
		ToolName:         "bwa",
		ImageUri:         "registry.example.com/bwa:1.0",
		Digest:           "sha256:abc",
		Version:          "0.7.17",
		EnvironmentSpec:  "name: bwa\ndependencies:\n  - bwa=0.7.17=h5bf99c6_8\n",
		Inputs: []*nfv1.PortSpec{
			{Name: "reads.fq", Role: "sample-fastq", Format: "fastq", Shape: "pair", Required: true},
		},
		Outputs: []*nfv1.PortSpec{
			{Name: "aligned.bam", Role: "aligned-bam", Format: "bam", Shape: "single", Class: "primary"},
		},
		Display: &nfv1.DisplaySpec{
			Label:    "BWA 0.7.17",
			Category: "Alignment",
		},
	}

	resp, err := svc.RegisterTool(t.Context(), req)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if resp.CasHash == "" {
		t.Error("RegisterTool returned empty CasHash")
	}
	if resp.Tool == nil {
		t.Fatal("RegisterTool returned nil Tool")
	}
	if resp.Tool.CasHash != resp.CasHash {
		t.Errorf("Tool.CasHash %q != response CasHash %q", resp.Tool.CasHash, resp.CasHash)
	}
	if resp.Tool.EnvironmentSpec != req.EnvironmentSpec {
		t.Errorf("Tool.EnvironmentSpec %q != request EnvironmentSpec %q", resp.Tool.EnvironmentSpec, req.EnvironmentSpec)
	}
}

// TestListTools_AfterRegister verifies ListTools returns previously registered tools.
// Each RegisterTool now writes exactly one file (SaveWithCasHash), so exactly N tools expected.
func TestListTools_AfterRegister(t *testing.T) {
	svc := newTestService(t)

	for i, name := range []string{"star", "salmon"} {
		_, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
			RequestId: "req-" + string(rune('0'+i)),
			ToolName:  name,
			Digest:    "sha256:000",
		})
		if err != nil {
			t.Fatalf("RegisterTool %s: %v", name, err)
		}
	}

	resp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(resp.Tools) != 2 {
		t.Errorf("ListTools returned %d tools, want exactly 2", len(resp.Tools))
	}
	names := make(map[string]struct{})
	for _, tool := range resp.Tools {
		names[tool.ToolName] = struct{}{}
	}
	for _, want := range []string{"star", "salmon"} {
		if _, ok := names[want]; !ok {
			t.Errorf("ListTools missing tool %q", want)
		}
	}
}

// TestRegisterTool_V02RoundTrip verifies that all v0.2 fields survive the
// RegisterTool → GetTool round-trip through CAS storage.
//
//nolint:gocyclo // comprehensive field-by-field round-trip assertion — splitting would reduce readability.
func TestRegisterTool_V02RoundTrip(t *testing.T) {
	svc := newTestService(t)

	req := &nfv1.RegisterToolRequest{
		RequestId:        "req-v02",
		ToolDefinitionId: "def-v02",
		ToolName:         "bwa-mem2",
		Version:          "2.2.1",
		ImageUri:         "registry.example.com/bwa-mem2:2.2.1@sha256:deadbeef",
		Digest:           "sha256:deadbeef",
		EnvironmentSpec:  "name: bwa\ndependencies:\n  - bwa-mem2=2.2.1\n",
		Command:          "/usr/bin/bwa-mem2",
		Inputs: []*nfv1.PortSpec{
			{Name: "reads", Role: "sample-fastq", Format: "fastq", Shape: "pair", Required: true},
		},
		Outputs: []*nfv1.PortSpec{
			{Name: "aligned", Role: "aligned-bam", Format: "bam", Shape: "single", Class: "primary",
				Constraints: map[string]string{"sorted": "coordinate"}},
		},
		Display: &nfv1.DisplaySpec{
			Label:       "BWA-MEM2 2.2.1",
			Description: "Fast aligner",
			Category:    "Alignment",
			Tags:        []string{"wgs", "alignment"},
		},
	}

	regResp, err := svc.RegisterTool(t.Context(), req)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if regResp.CasHash == "" {
		t.Fatal("empty CasHash")
	}

	got, err := svc.GetTool(t.Context(), &nfv1.GetToolRequest{CasHash: regResp.CasHash})
	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	// ── v0.2 field round-trip assertions ────────────────────────────────────
	if got.CasHash != regResp.CasHash {
		t.Errorf("CasHash: got %q want %q", got.CasHash, regResp.CasHash)
	}
	if got.ToolDefinitionId != req.ToolDefinitionId {
		t.Errorf("ToolDefinitionId: got %q want %q", got.ToolDefinitionId, req.ToolDefinitionId)
	}
	if got.ToolName != req.ToolName {
		t.Errorf("ToolName: got %q want %q", got.ToolName, req.ToolName)
	}
	if got.Version != req.Version {
		t.Errorf("Version: got %q want %q", got.Version, req.Version)
	}
	wantStableRef := req.ToolName + "@" + req.Version
	if got.StableRef != wantStableRef {
		t.Errorf("StableRef: got %q want %q", got.StableRef, wantStableRef)
	}
	if got.ImageUri != req.ImageUri {
		t.Errorf("ImageUri: got %q want %q", got.ImageUri, req.ImageUri)
	}
	if got.Digest != req.Digest {
		t.Errorf("Digest: got %q want %q", got.Digest, req.Digest)
	}
	if got.EnvironmentSpec != req.EnvironmentSpec {
		t.Errorf("EnvironmentSpec mismatch")
	}
	if got.Command != req.Command {
		t.Errorf("Command: got %q want %q", got.Command, req.Command)
	}
	if got.LifecyclePhase != "Active" {
		t.Errorf("LifecyclePhase: got %q want Active", got.LifecyclePhase)
	}
	if got.IntegrityHealth != string(index.HealthPartial) {
		t.Errorf("IntegrityHealth: got %q want %q (Partial until spec referrer pushed)", got.IntegrityHealth, index.HealthPartial)
	}
	if got.RegisteredAt == 0 {
		t.Error("RegisteredAt should be non-zero")
	}
	if got.Validation == nil || got.Validation.Phase != "Passed" {
		t.Errorf("Validation.Phase: got %v want Passed", got.Validation)
	}
	if len(got.Inputs) != 1 || got.Inputs[0].Name != "reads" {
		t.Errorf("Inputs mismatch")
	}
	if len(got.Outputs) != 1 || got.Outputs[0].Name != "aligned" {
		t.Errorf("Outputs mismatch")
	}
	if got.Outputs[0].Constraints["sorted"] != "coordinate" {
		t.Errorf("Outputs[0].Constraints[sorted] mismatch")
	}
	if got.Display == nil || got.Display.Label != "BWA-MEM2 2.2.1" {
		t.Errorf("Display.Label mismatch")
	}
	if len(got.Display.Tags) != 2 {
		t.Errorf("Display.Tags: got %d want 2", len(got.Display.Tags))
	}
}

// TestRegisterTool_SingleFilePerRegistration verifies SaveWithCasHash writes
// exactly one .tooldefinition file (no ghost files from double-save).
func TestRegisterTool_SingleFilePerRegistration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CATALOG_DIR", dir)
	cat := catalog.NewCatalog()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	_, err = svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "bowtie2",
		Digest:   "sha256:abc",
		Version:  "2.5.0",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	if len(files) != 1 {
		t.Errorf("expected exactly 1 .tooldefinition file, got %d: %v", len(files), files)
	}
}

// TestListTools_StableRefFilter verifies that ListTools(stable_ref=X) returns
// only tools matching X and ignores others.
func TestListTools_StableRefFilter(t *testing.T) {
	svc := newTestService(t)

	// Register two tools: bwa@1.0 and bowtie2@2.0
	for _, tc := range []struct{ name, version string }{
		{"bwa", "1.0"},
		{"bowtie2", "2.0"},
	} {
		if _, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
			ToolName: tc.name,
			Version:  tc.version,
			Digest:   "sha256:000",
		}); err != nil {
			t.Fatalf("RegisterTool %s: %v", tc.name, err)
		}
	}

	resp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{StableRef: "bwa@1.0"})
	if err != nil {
		t.Fatalf("ListTools with filter: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Fatalf("expected 1 tool for stable_ref=bwa@1.0, got %d", len(resp.Tools))
	}
	if resp.Tools[0].ToolName != "bwa" {
		t.Errorf("expected bwa, got %q", resp.Tools[0].ToolName)
	}

	// Empty filter returns all Active tools
	allResp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools all: %v", err)
	}
	if len(allResp.Tools) != 2 {
		t.Errorf("expected 2 tools total, got %d", len(allResp.Tools))
	}
}

// TestListTools_ArtifactKindFilter verifies that artifact_kind filter works.
func TestListTools_ArtifactKindFilter(t *testing.T) {
	svc := newTestService(t)

	// Register a tool (artifact_kind = "tool" by default)
	if _, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "bwa",
		Version:  "1.0",
		Digest:   "sha256:abc",
	}); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	// Filter for "tool" kind — must return 1 result
	resp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{ArtifactKind: "tool"})
	if err != nil {
		t.Fatalf("ListTools kind=tool: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(resp.Tools))
	}

	// Filter for "data" kind — must return 0 results
	dataResp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{ArtifactKind: "data"})
	if err != nil {
		t.Fatalf("ListTools kind=data: %v", err)
	}
	if len(dataResp.Tools) != 0 {
		t.Errorf("expected 0 data tools, got %d", len(dataResp.Tools))
	}
}

// TestGetTool_NotFound verifies GetTool returns NotFound for unknown casHash.
func TestGetTool_NotFound(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.GetTool(t.Context(), &nfv1.GetToolRequest{CasHash: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent casHash")
	}
}

// TestRetractTool_TransitionsPhase verifies lifecycle_phase → Retracted and Catalog exclusion.
func TestRetractTool_TransitionsPhase(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "star",
		Version:  "2.7.11",
		Digest:   "sha256:aaa",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	retResp, err := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{
		CasHash: reg.CasHash,
		Reason:  "security issue",
	})
	if err != nil {
		t.Fatalf("RetractTool: %v", err)
	}
	if retResp.LifecyclePhase != "Retracted" {
		t.Errorf("LifecyclePhase: got %q want Retracted", retResp.LifecyclePhase)
	}

	// ListActive must not include the retracted tool.
	listResp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range listResp.Tools {
		if tool.CasHash == reg.CasHash {
			t.Error("retracted tool must not appear in ListTools (Active filter)")
		}
	}
}

// TestRetractTool_NotFound verifies RetractTool returns NotFound for unknown casHash.
func TestRetractTool_NotFound(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: "nonexistent"})
	if err == nil {
		t.Fatal("expected NotFound error")
	}
}

// TestDeleteTool_TransitionsPhase verifies lifecycle_phase → Deleted.
func TestDeleteTool_TransitionsPhase(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "hisat2",
		Version:  "2.2.1",
		Digest:   "sha256:bbb",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	// Retract first (recommended sequence: Active → Retracted → Deleted).
	_, err = svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: reg.CasHash})
	if err != nil {
		t.Fatalf("RetractTool: %v", err)
	}

	delResp, err := svc.DeleteTool(t.Context(), &nfv1.DeleteToolRequest{
		CasHash: reg.CasHash,
		Reason:  "permanent removal",
	})
	if err != nil {
		t.Fatalf("DeleteTool: %v", err)
	}
	if delResp.LifecyclePhase != "Deleted" {
		t.Errorf("LifecyclePhase: got %q want Deleted", delResp.LifecyclePhase)
	}

	// Deleted tool must not appear in ListTools either.
	listResp, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range listResp.Tools {
		if tool.CasHash == reg.CasHash {
			t.Error("deleted tool must not appear in ListTools")
		}
	}
}

// TestRetractTool_IntegrityHealthUnchanged verifies Retract does NOT touch integrity_health.
// The two state axes must remain independent.
func TestRetractTool_IntegrityHealthUnchanged(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "bwa",
		Version:  "0.7.17",
		Digest:   "sha256:ccc",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	// Manually set integrity_health to Partial (simulating reconcile observation).
	if setErr := store.SetIntegrityHealth(reg.CasHash, index.HealthPartial); setErr != nil {
		t.Fatalf("SetIntegrityHealth: %v", setErr)
	}

	// Retract — must NOT change integrity_health.
	_, err = svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: reg.CasHash})
	if err != nil {
		t.Fatalf("RetractTool: %v", err)
	}

	entry, err := store.GetByCasHash(reg.CasHash)
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if entry.LifecyclePhase != index.PhaseRetracted {
		t.Errorf("LifecyclePhase: got %q want Retracted", entry.LifecyclePhase)
	}
	if entry.IntegrityHealth != index.HealthPartial {
		t.Errorf("IntegrityHealth must remain Partial after Retract, got %q", entry.IntegrityHealth)
	}
}

// TestRegisterTool_IndexDualWrite verifies that RegisterTool appends an entry to the index.
func TestRegisterTool_IndexDualWrite(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	resp, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "hisat2",
		Version:  "2.2.1",
		Digest:   "sha256:abc",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	// Index entry must exist with matching casHash.
	entry, indexErr := store.GetByCasHash(resp.CasHash)
	if indexErr != nil {
		t.Fatalf("index.GetByCasHash: %v", indexErr)
	}
	if entry.CasHash != resp.CasHash {
		t.Errorf("index entry CasHash: got %q want %q", entry.CasHash, resp.CasHash)
	}
	if entry.StableRef != "hisat2@2.2.1" {
		t.Errorf("index entry StableRef: got %q want hisat2@2.2.1", entry.StableRef)
	}
	if entry.LifecyclePhase != index.PhaseActive {
		t.Errorf("index entry LifecyclePhase: got %q want Active", entry.LifecyclePhase)
	}
	if entry.IntegrityHealth != index.HealthPartial {
		t.Errorf("index entry IntegrityHealth: got %q want %q (Partial until spec referrer pushed)", entry.IntegrityHealth, index.HealthPartial)
	}
}
