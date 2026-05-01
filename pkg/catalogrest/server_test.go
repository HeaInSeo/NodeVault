package catalogrest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// ── test helpers ──────────────────────────────────────────────────────────────

func newTestDeps(t *testing.T) (*index.Store, *catalog.Catalog) {
	t.Helper()
	t.Setenv("CATALOG_DIR", t.TempDir())
	cat := catalog.NewCatalog()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return store, cat
}

func registerTool(t *testing.T, svc *catalog.ToolRegistryService, name, version string) string {
	t.Helper()
	resp, err := svc.RegisterTool(context.Background(), &nfv1.RegisterToolRequest{
		ToolName: name,
		Version:  version,
		Digest:   "sha256:abc",
		Display: &nfv1.DisplaySpec{
			Label:    name + " " + version,
			Category: "Test",
		},
	})
	if err != nil {
		t.Fatalf("RegisterTool %s: %v", name, err)
	}
	return resp.CasHash
}

func newServer(t *testing.T) (*httptest.Server, *catalog.ToolRegistryService) {
	t.Helper()
	store, cat := newTestDeps(t)
	svc := catalog.NewToolRegistryService(cat, store)
	dataCat := catalog.NewDataCatalogAt(t.TempDir())
	mux := catalogrest.NewMux(store, cat, dataCat)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, svc
}

// doGet issues a GET request with context and fatals on transport error.
func doGet(t *testing.T, ts *httptest.Server, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build GET request: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// ── GET /v1/catalog/tools ─────────────────────────────────────────────────────

func TestListTools_Empty(t *testing.T) {
	ts, _ := newServer(t)

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var body catalogrest.ListToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 0 {
		t.Errorf("expected empty tools, got %d", len(body.Tools))
	}
}

func TestListTools_ReturnsActiveTools(t *testing.T) {
	ts, svc := newServer(t)

	registerTool(t, svc, "bwa", "1.0")
	registerTool(t, svc, "samtools", "1.17")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools")
	defer func() { _ = resp.Body.Close() }()

	var body catalogrest.ListToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(body.Tools))
	}
}

func TestListTools_StableRefFilter(t *testing.T) {
	ts, svc := newServer(t)

	registerTool(t, svc, "bwa", "1.0")
	registerTool(t, svc, "bowtie2", "2.0")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools?stable_ref=bwa@1.0")
	defer func() { _ = resp.Body.Close() }()

	var body catalogrest.ListToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 1 {
		t.Fatalf("expected 1 tool for stable_ref=bwa@1.0, got %d", len(body.Tools))
	}
	if body.Tools[0].ToolName != "bwa" {
		t.Errorf("expected bwa, got %q", body.Tools[0].ToolName)
	}
}

func TestListTools_ArtifactKindFilter(t *testing.T) {
	ts, svc := newServer(t)
	registerTool(t, svc, "bwa", "1.0")

	// filter tool kind → 1 result
	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools?artifact_kind=tool")
	defer func() { _ = resp.Body.Close() }()
	var toolBody catalogrest.ListToolsResponse
	_ = json.NewDecoder(resp.Body).Decode(&toolBody)
	if len(toolBody.Tools) != 1 {
		t.Errorf("artifact_kind=tool: expected 1, got %d", len(toolBody.Tools))
	}

	// filter data kind → 0 results
	resp2 := doGet(t, ts, ts.URL+"/v1/catalog/tools?artifact_kind=data")
	defer func() { _ = resp2.Body.Close() }()
	var dataBody catalogrest.ListToolsResponse
	_ = json.NewDecoder(resp2.Body).Decode(&dataBody)
	if len(dataBody.Tools) != 0 {
		t.Errorf("artifact_kind=data: expected 0, got %d", len(dataBody.Tools))
	}
}

func TestListTools_ContentType(t *testing.T) {
	ts, _ := newServer(t)

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools")
	defer func() { _ = resp.Body.Close() }()

	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", ct)
	}
}

// ── GET /v1/catalog/tools/{cas_hash} ─────────────────────────────────────────

func TestGetTool_Found(t *testing.T) {
	ts, svc := newServer(t)
	hash := registerTool(t, svc, "hisat2", "2.2.1")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools/"+hash)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var item catalogrest.ToolItem
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if item.CasHash != hash {
		t.Errorf("CasHash: got %q want %q", item.CasHash, hash)
	}
	if item.ToolName != "hisat2" {
		t.Errorf("ToolName: got %q want hisat2", item.ToolName)
	}
	if item.StableRef != "hisat2@2.2.1" {
		t.Errorf("StableRef: got %q want hisat2@2.2.1", item.StableRef)
	}
	if item.LifecyclePhase != "Active" {
		t.Errorf("LifecyclePhase: got %q want Active", item.LifecyclePhase)
	}
	if item.DisplayLabel != "hisat2 2.2.1" {
		t.Errorf("DisplayLabel: got %q want 'hisat2 2.2.1'", item.DisplayLabel)
	}
}

func TestGetTool_NotFound(t *testing.T) {
	ts, _ := newServer(t)

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools/nonexistent")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d want 404", resp.StatusCode)
	}
}

// ── ToolItem field completeness ───────────────────────────────────────────────

func TestToolItem_RegisteredAt_NonZero(t *testing.T) {
	ts, svc := newServer(t)
	now := time.Now().Unix()
	hash := registerTool(t, svc, "star", "2.7.11")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools/"+hash)
	defer func() { _ = resp.Body.Close() }()
	var item catalogrest.ToolItem
	_ = json.NewDecoder(resp.Body).Decode(&item)

	if item.RegisteredAt < now-5 || item.RegisteredAt > now+5 {
		t.Errorf("RegisteredAt %d seems wrong (expected ~%d)", item.RegisteredAt, now)
	}
}

func TestToolItem_IntegrityHealth_Default(t *testing.T) {
	ts, svc := newServer(t)
	hash := registerTool(t, svc, "bwa", "1.0")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/tools/"+hash)
	defer func() { _ = resp.Body.Close() }()
	var item catalogrest.ToolItem
	_ = json.NewDecoder(resp.Body).Decode(&item)

	if item.IntegrityHealth != "Partial" {
		t.Errorf("IntegrityHealth: got %q want Partial (Partial until spec referrer pushed)", item.IntegrityHealth)
	}
}
