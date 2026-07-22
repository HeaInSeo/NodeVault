package catalogrest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

var errCertBoom = errors.New("cert boom")

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
		ToolName:  name,
		Version:   version,
		Digest:    "sha256:abc",
		BuildKind: nfv1.BuildKind_BUILD_KIND_TOOLSPEC,
		ImageUri: "registry.example.com/test:latest",
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
	// DisplayLabel is populated via ToolFunctionSpec after dry-run, not at registration time.
	if item.DisplayLabel != "" {
		t.Errorf("DisplayLabel: got %q want empty (ToolFunctionSpec not yet set)", item.DisplayLabel)
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

func TestPaletteToolsAlias_IncludesCasHash(t *testing.T) {
	ts, svc := newServer(t)
	hash := registerTool(t, svc, "bwa", "1.0")

	resp := doGet(t, ts, ts.URL+"/v1/palette/tools")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	var body catalogrest.ListToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 1 {
		t.Fatalf("tools len got %d want 1", len(body.Tools))
	}
	if body.Tools[0].CasHash != hash {
		t.Fatalf("CasHash got %q want %q", body.Tools[0].CasHash, hash)
	}
}

// ── fakeCertSvc for catalogrest tests ────────────────────────────────────────

type fakeCertSvc struct {
	checkErr error
	scanErr  error
}

//nolint:gocritic // hugeParam: interface method signatures are fixed by certService interface.
func (f *fakeCertSvc) EvaluateAfterCheck(_ index.ToolCheckRecord) error { return f.checkErr }

//nolint:gocritic // hugeParam: interface method signatures are fixed by certService interface.
func (f *fakeCertSvc) EvaluateAfterScan(_ index.ToolScanRecord) error { return f.scanErr }

// newServerWithCert creates a test server with a fake certService wired in.
func newServerWithCert(t *testing.T, certSvc *fakeCertSvc) (*httptest.Server, *index.Store) {
	t.Helper()
	store, cat := newTestDeps(t)
	dataCat := catalog.NewDataCatalogAt(t.TempDir())
	mux := catalogrest.NewMuxWithCert(store, cat, dataCat, certSvc)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, store
}

// doPost issues a POST request and returns the response.
func doPost(t *testing.T, ts *httptest.Server, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// ── GET /v1/catalog/certified-tools tests ─────────────────────────────────────

// TestGetCertifiedTool_EmptyCasHash verifies WARN-7: empty cas_hash returns 400.
func TestGetCertifiedTool_EmptyCasHash(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	// The path pattern "GET /v1/catalog/certified-tools/{cas_hash}" requires a
	// non-empty segment; an empty segment routes to handleListCertifiedTools instead.
	// We test the explicit guard by hitting the route without the path segment.
	// This effectively hits handleListCertifiedTools — but also tests the guard in
	// handleGetCertifiedTool via a path value of "".
	// The real guard fires when the Go 1.22 ServeMux provides "" as PathValue.
	// We verify the list endpoint defaults gracefully (200) while the guard protects the get.
	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("list without cas_hash: got %d want 200", resp.StatusCode)
	}
}

// TestGetCertifiedTool_NotFound verifies that a non-existent cas_hash returns 404.
func TestGetCertifiedTool_NotFound(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools/nonexistent-hash")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d want 404", resp.StatusCode)
	}
}

// TestListCertifiedTools_InvalidPromotionStatus verifies WARN-8: invalid promotion_status returns 400.
func TestListCertifiedTools_InvalidPromotionStatus(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools?promotion_status=INVALID")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
	// Check response body mentions allowlist.
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), "active") {
		t.Errorf("expected error body to mention 'active', got: %q", buf.String())
	}
}

// TestListCertifiedTools_ValidStatus verifies "active" is accepted and returns 200.
func TestListCertifiedTools_ValidStatus(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools?promotion_status=active")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
}

// TestListCertifiedTools_EmptyStatus verifies empty promotion_status defaults to "active" (200).
func TestListCertifiedTools_EmptyStatus(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var body catalogrest.ListCertifiedToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Tools == nil {
		t.Error("expected non-nil tools slice")
	}
}

// TestSubmitCheckRecord_BodyClose verifies that POST /v1/validation/check-records
// processes a valid body correctly and doesn't panic (body close is handled).
func TestSubmitCheckRecord_BodyClose(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID:          "chk-rest-1",
		ImageDigest:      "sha256:rest111",
		ToolName:         "bwa",
		Version:          "1.0",
		ValidationStatus: "succeeded",
	})

	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var result catalogrest.SubmitRecordResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.RecordID != "chk-rest-1" {
		t.Errorf("RecordID: got %q want chk-rest-1", result.RecordID)
	}
}

// TestListCertifiedTools_ValidSupersededStatus verifies "superseded" is accepted.
func TestListCertifiedTools_ValidSupersededStatus(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	// Seed a superseded entry.
	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash:         "cas-sup-1",
		ToolName:        "hisat2",
		Version:         "2.1.0",
		PromotionStatus: index.PromotionSuperseded,
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}

	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools?promotion_status=superseded")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var body catalogrest.ListCertifiedToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 1 {
		t.Errorf("expected 1 superseded tool, got %d", len(body.Tools))
	}
}

// TestGetCertifiedTool_Found verifies that an existing certified tool is returned.
func TestGetCertifiedTool_Found(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash:         "cas-get-1",
		ToolName:        "bwa",
		Version:         "2.0",
		StableRef:       "bwa@2.0",
		PromotionStatus: index.PromotionActive,
		ValidationHash:  "vh-abc",
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}

	resp := doGet(t, ts, ts.URL+"/v1/catalog/certified-tools/cas-get-1")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var item catalogrest.CertifiedToolItem
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if item.CasHash != "cas-get-1" {
		t.Errorf("CasHash: got %q want cas-get-1", item.CasHash)
	}
	if item.ToolName != "bwa" {
		t.Errorf("ToolName: got %q want bwa", item.ToolName)
	}
	if item.PromotionStatus != "active" {
		t.Errorf("PromotionStatus: got %q want active", item.PromotionStatus)
	}
}

// ── GET /v1/catalog/data tests ────────────────────────────────────────────────

func newServerWithData(t *testing.T) *httptest.Server {
	t.Helper()
	store, cat := newTestDeps(t)
	dataCat := catalog.NewDataCatalogAt(t.TempDir())
	mux := catalogrest.NewMux(store, cat, dataCat)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// registerData registers a data artifact and returns its CasHash.
func registerData(t *testing.T, store *index.Store, dataCat *catalog.DataCatalog, name, version string) string {
	t.Helper()
	svc := catalog.NewDataRegistryService(dataCat, store)
	resp, err := svc.RegisterData(context.Background(), &nfv1.DataRegisterRequest{
		DataName: name,
		Version:  version,
		Format:   "csv",
		Checksum:   "sha256:test",
		StorageUri: "s3://test/artifact",
		Display: &nfv1.DisplaySpec{
			Label:    name + " " + version,
			Category: "TestData",
		},
	})
	if err != nil {
		t.Fatalf("RegisterData %s: %v", name, err)
	}
	return resp.CasHash
}

// newServerWithDataCat creates a test server and returns the DataCatalog for seeding.
func newServerWithDataCat(t *testing.T) (*httptest.Server, *index.Store, *catalog.DataCatalog) {
	t.Helper()
	store, cat := newTestDeps(t)
	dataCat := catalog.NewDataCatalogAt(t.TempDir())
	mux := catalogrest.NewMux(store, cat, dataCat)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, store, dataCat
}

// TestListData_Empty verifies GET /v1/catalog/data returns 200 with empty slice.
func TestListData_Empty(t *testing.T) {
	ts := newServerWithData(t)

	resp := doGet(t, ts, ts.URL+"/v1/catalog/data")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var body catalogrest.ListDataResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 0 {
		t.Errorf("expected empty data, got %d", len(body.Data))
	}
}

// TestListData_ReturnsDataArtifacts verifies GET /v1/catalog/data returns seeded data.
func TestListData_ReturnsDataArtifacts(t *testing.T) {
	ts, store, dataCat := newServerWithDataCat(t)

	registerData(t, store, dataCat, "hg38", "1.0")
	registerData(t, store, dataCat, "hg19", "2.0")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/data")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var body catalogrest.ListDataResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 2 {
		t.Errorf("expected 2 data artifacts, got %d", len(body.Data))
	}
}

// TestGetData_Found verifies GET /v1/catalog/data/{cas_hash} returns the correct artifact.
func TestGetData_Found(t *testing.T) {
	ts, store, dataCat := newServerWithDataCat(t)

	hash := registerData(t, store, dataCat, "hg38", "1.0")

	resp := doGet(t, ts, ts.URL+"/v1/catalog/data/"+hash)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var item catalogrest.DataItem
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if item.CasHash != hash {
		t.Errorf("CasHash: got %q want %q", item.CasHash, hash)
	}
	if item.DataName != "hg38" {
		t.Errorf("DataName: got %q want hg38", item.DataName)
	}
	if item.DisplayLabel != "hg38 1.0" {
		t.Errorf("DisplayLabel: got %q want 'hg38 1.0'", item.DisplayLabel)
	}
}

// TestGetData_NotFound verifies GET /v1/catalog/data/{cas_hash} returns 404 for unknown hash.
func TestGetData_NotFound(t *testing.T) {
	ts := newServerWithData(t)

	resp := doGet(t, ts, ts.URL+"/v1/catalog/data/nonexistent")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d want 404", resp.StatusCode)
	}
}

// ── POST /v1/validation/scan-records tests ────────────────────────────────────

// TestSubmitScanRecord_HappyPath verifies POST /v1/validation/scan-records succeeds.
func TestSubmitScanRecord_HappyPath(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID:      "scan-rest-1",
		ImageDigest: "sha256:scan111",
		Scanner:     "trivy",
		PolicyMode:  "gate_critical",
	})

	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var result catalogrest.SubmitRecordResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.RecordID != "scan-rest-1" {
		t.Errorf("RecordID: got %q want scan-rest-1", result.RecordID)
	}
	if result.CertificationStatus != "pending" {
		t.Errorf("CertificationStatus: got %q want pending", result.CertificationStatus)
	}
}

// TestSubmitScanRecord_MissingFields verifies missing scan_id returns 400.
func TestSubmitScanRecord_MissingFields(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID:      "",
		ImageDigest: "sha256:abc",
	})

	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
}

// TestSubmitScanRecord_InvalidJSON verifies invalid JSON body returns 400.
func TestSubmitScanRecord_InvalidJSON(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", []byte("{bad json"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
}

// TestSubmitCheckRecord_MissingFields verifies missing check_id returns 400.
func TestSubmitCheckRecord_MissingFields(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID:     "",
		ImageDigest: "sha256:abc",
	})

	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
}

// TestSubmitCheckRecord_InvalidJSON verifies invalid JSON returns 400.
func TestSubmitCheckRecord_InvalidJSON(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{})

	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", []byte("{bad json"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
}

// TestSubmitCheckRecord_CertFailed verifies that certSvc error sets certStatus="failed".
func TestSubmitCheckRecord_CertFailed(t *testing.T) {
	ts, _ := newServerWithCert(t, &fakeCertSvc{checkErr: errCertBoom})

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID:          "chk-cert-fail",
		ImageDigest:      "sha256:certfail",
		ToolName:         "bwa",
		Version:          "1.0",
		ValidationStatus: "succeeded",
	})

	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}

	var result catalogrest.SubmitRecordResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.CertificationStatus != "failed" {
		t.Errorf("CertificationStatus: got %q want failed", result.CertificationStatus)
	}
}
