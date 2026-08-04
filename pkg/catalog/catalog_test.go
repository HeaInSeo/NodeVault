package catalog_test

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
		BuildKind:        nfv1.BuildKind_BUILD_KIND_TOOLSPEC,
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
			ImageUri:  "registry.example.com/test:latest",
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
		BuildKind:        nfv1.BuildKind_BUILD_KIND_TOOLSPEC,
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
	if got.BuildKind != nfv1.BuildKind_BUILD_KIND_TOOLSPEC {
		t.Errorf("BuildKind: got %v want BUILD_KIND_TOOLSPEC", got.BuildKind)
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
	if got.Command != "" || len(got.Inputs) != 0 || len(got.Outputs) != 0 || got.Display != nil {
		t.Fatalf("ToolFunctionSpec metadata should not be populated by build-time RegisterTool: %+v", got)
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
		ImageUri: "registry.example.com/test:latest",
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
			ImageUri: "registry.example.com/test:latest",
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
		ImageUri: "registry.example.com/test:latest",
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
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetTool error = %v, want codes.NotFound", err)
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
		ImageUri: "registry.example.com/test:latest",
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
	if status.Code(err) != codes.NotFound {
		t.Fatalf("RetractTool error = %v, want codes.NotFound", err)
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
		ImageUri: "registry.example.com/test:latest",
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
		ImageUri: "registry.example.com/test:latest",
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
		ImageUri: "registry.example.com/test:latest",
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

func TestToolRegistry_EmptyResourceID_InvalidArgument(t *testing.T) {
	svc := newTestService(t)
	tests := []struct {
		name string
		call func() error
	}{
		{name: "get", call: func() error {
			_, err := svc.GetTool(t.Context(), &nfv1.GetToolRequest{})
			return err
		}},
		{name: "retract", call: func() error {
			_, err := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{})
			return err
		}},
		{name: "delete", call: func() error {
			_, err := svc.DeleteTool(t.Context(), &nfv1.DeleteToolRequest{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("error = %v, want codes.InvalidArgument", err)
			}
		})
	}
}

func TestToolRegistry_UninitializedDependencies_Unavailable(t *testing.T) {
	svc := catalog.NewToolRegistryService(nil, nil)
	tests := []struct {
		name string
		call func() error
	}{
		{name: "register", call: func() error {
			_, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
				ToolName: "bwa",
				ImageUri: "registry.example.com/test:latest",
			})
			return err
		}},
		{name: "list", call: func() error {
			_, err := svc.ListTools(t.Context(), &nfv1.ListToolsRequest{})
			return err
		}},
		{name: "get", call: func() error {
			_, err := svc.GetTool(t.Context(), &nfv1.GetToolRequest{CasHash: "abc"})
			return err
		}},
		{name: "retract", call: func() error {
			_, err := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: "abc"})
			return err
		}},
		{name: "delete", call: func() error {
			_, err := svc.DeleteTool(t.Context(), &nfv1.DeleteToolRequest{CasHash: "abc"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); status.Code(err) != codes.Unavailable {
				t.Fatalf("error = %v, want codes.Unavailable", err)
			}
		})
	}
}

func TestRegisterTool_RequiredFields_InvalidArgument(t *testing.T) {
	svc := newTestService(t)
	tests := []struct {
		name string
		req  *nfv1.RegisterToolRequest
	}{
		{name: "tool name", req: &nfv1.RegisterToolRequest{ImageUri: "registry.example.com/tool:1", Digest: "sha256:abc"}},
		{name: "image uri", req: &nfv1.RegisterToolRequest{ToolName: "tool", Digest: "sha256:abc"}},
		{name: "digest", req: &nfv1.RegisterToolRequest{ToolName: "tool", ImageUri: "registry.example.com/tool:1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.RegisterTool(t.Context(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("RegisterTool error = %v, want codes.InvalidArgument", err)
			}
		})
	}
}

func TestGetTool_IndexPresentCASMissing_DataLoss(t *testing.T) {
	dir := t.TempDir()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(catalog.NewCatalogAt(dir), store)
	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "bwa",
		ImageUri: "registry.example.com/bwa:1",
		Digest:   "sha256:abc",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if removeErr := os.Remove(filepath.Join(dir, reg.GetCasHash()+".tooldefinition")); removeErr != nil {
		t.Fatalf("remove CAS object: %v", removeErr)
	}
	_, err = svc.GetTool(t.Context(), &nfv1.GetToolRequest{CasHash: reg.GetCasHash()})
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("GetTool error = %v, want codes.DataLoss", err)
	}
}

// TestRetractTool_Idempotent verifies that calling RetractTool twice succeeds.
// The store transition table is strict (Retracted → Retracted is rejected as a
// self-edge), so command-level idempotency lives in the service: the second call
// observes the phase is already Retracted and returns success without a store
// write. LifecycleUpdatedAt must be unchanged by the second call, proving no-op.
func TestRetractTool_Idempotent(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "salmon",
		Version:  "1.10.0",
		Digest:   "sha256:ddd",
		ImageUri: "registry.example.com/test:latest",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	first, err := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: reg.CasHash})
	if err != nil {
		t.Fatalf("RetractTool (first): %v", err)
	}
	if first.LifecyclePhase != string(index.PhaseRetracted) {
		t.Errorf("first LifecyclePhase: got %q want Retracted", first.LifecyclePhase)
	}
	entryAfterFirst, err := store.GetByCasHash(reg.CasHash)
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}

	second, err := svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: reg.CasHash})
	if err != nil {
		t.Fatalf("RetractTool (second) must be idempotent success, got: %v", err)
	}
	if second.LifecyclePhase != string(index.PhaseRetracted) {
		t.Errorf("second LifecyclePhase: got %q want Retracted", second.LifecyclePhase)
	}
	entryAfterSecond, err := store.GetByCasHash(reg.CasHash)
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if !entryAfterSecond.LifecycleUpdatedAt.Equal(entryAfterFirst.LifecycleUpdatedAt) {
		t.Errorf("idempotent RetractTool must not write the store: LifecycleUpdatedAt changed %v -> %v",
			entryAfterFirst.LifecycleUpdatedAt, entryAfterSecond.LifecycleUpdatedAt)
	}
}

// TestDeleteTool_Idempotent verifies that calling DeleteTool twice succeeds once
// the tool is Deleted (tombstone present). Deleted → Deleted is a rejected
// self-edge in the store, so the second success is command-level idempotency:
// the phase is already Deleted and the entry (tombstone) still exists.
func TestDeleteTool_Idempotent(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "kallisto",
		Version:  "0.50.1",
		Digest:   "sha256:eee",
		ImageUri: "registry.example.com/test:latest",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	// Required sequence: Active → Retracted → Deleted.
	if _, err = svc.RetractTool(t.Context(), &nfv1.RetractToolRequest{CasHash: reg.CasHash}); err != nil {
		t.Fatalf("RetractTool: %v", err)
	}
	if _, err = svc.DeleteTool(t.Context(), &nfv1.DeleteToolRequest{CasHash: reg.CasHash}); err != nil {
		t.Fatalf("DeleteTool (first): %v", err)
	}
	entryAfterFirst, err := store.GetByCasHash(reg.CasHash)
	if err != nil {
		t.Fatalf("GetByCasHash (tombstone must exist): %v", err)
	}

	second, err := svc.DeleteTool(t.Context(), &nfv1.DeleteToolRequest{CasHash: reg.CasHash})
	if err != nil {
		t.Fatalf("DeleteTool (second) must be idempotent success, got: %v", err)
	}
	if second.LifecyclePhase != string(index.PhaseDeleted) {
		t.Errorf("second LifecyclePhase: got %q want Deleted", second.LifecyclePhase)
	}
	entryAfterSecond, err := store.GetByCasHash(reg.CasHash)
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if !entryAfterSecond.LifecycleUpdatedAt.Equal(entryAfterFirst.LifecycleUpdatedAt) {
		t.Errorf("idempotent DeleteTool must not write the store: LifecycleUpdatedAt changed %v -> %v",
			entryAfterFirst.LifecycleUpdatedAt, entryAfterSecond.LifecycleUpdatedAt)
	}
}

// TestDeleteTool_ActiveForbidden_StillFails guards that command-level idempotency
// did not relax the forbidden Active → Deleted edge. Idempotency applies only when
// the tool is already at the target phase; skipping Retracted must still fail with
// FailedPrecondition.
func TestDeleteTool_ActiveForbidden_StillFails(t *testing.T) {
	cat := newTestCatalog(t)
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := catalog.NewToolRegistryService(cat, store)

	reg, err := svc.RegisterTool(t.Context(), &nfv1.RegisterToolRequest{
		ToolName: "minimap2",
		Version:  "2.28",
		Digest:   "sha256:fff",
		ImageUri: "registry.example.com/test:latest",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	// Active → Deleted (skipping Retracted) is forbidden.
	_, err = svc.DeleteTool(t.Context(), &nfv1.DeleteToolRequest{CasHash: reg.CasHash})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteTool on Active tool = %v, want codes.FailedPrecondition", err)
	}

	// The entry must remain Active — the rejected call performed no write.
	entry, err := store.GetByCasHash(reg.CasHash)
	if err != nil {
		t.Fatalf("GetByCasHash: %v", err)
	}
	if entry.LifecyclePhase != index.PhaseActive {
		t.Errorf("LifecyclePhase after forbidden delete: got %q want Active", entry.LifecyclePhase)
	}
}
