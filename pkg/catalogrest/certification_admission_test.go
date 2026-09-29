package catalogrest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

const certStatusPending = "pending"

// newServerWithRealCert wires the production certification.Service so the
// REST intake → certification path is exercised end to end (NodeVault #117).
func newServerWithRealCert(t *testing.T) (*httptest.Server, *index.Store) {
	t.Helper()
	store, cat := newTestDeps(t)
	dataCat := catalog.NewDataCatalogAt(t.TempDir())
	mux := catalogrest.NewMuxWithCert(store, cat, dataCat, certification.New(store))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, store
}

func postCheckRecord(t *testing.T, ts *httptest.Server, req *catalogrest.SubmitCheckRecordRequest) string {
	t.Helper()
	body, _ := json.Marshal(req)
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var result catalogrest.SubmitRecordResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return result.CertificationStatus
}

func seedRegisteredImage(t *testing.T, store *index.Store, casHash, imageDigest string) {
	t.Helper()
	if err := store.Append(index.Entry{
		CasHash:         casHash,
		ArtifactKind:    index.KindTool,
		StableRef:       "bwa@1.0",
		ToolName:        "bwa",
		Version:         "1.0",
		ImageDigest:     imageDigest,
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}); err != nil {
		t.Fatalf("Append index entry: %v", err)
	}
}

// TestSubmitCheckRecord_PlaceholderL5A_LeavesToolNonActive reproduces #117:
// NodeSentinel's stub L5-a ("/bin/sh -c true") submits a terminal succeeded
// record with no validation hash and outputs not observed. It must be stored
// but must not certify the tool.
func TestSubmitCheckRecord_PlaceholderL5A_LeavesToolNonActive(t *testing.T) {
	ts, store := newServerWithRealCert(t)
	seedRegisteredImage(t, store, "cas-ph", "sha256:ph")
	seedQueuedValidationRequest(t, store, "vr-ph", "sha256:ph")

	got := postCheckRecord(t, ts, &catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-ph", ImageDigest: "sha256:ph", ToolName: "bwa", Version: "1.0",
		ValidationRequestID: "vr-ph", SentinelJobID: "job-ph", Stage: "L5A", Terminal: true,
		ValidationStatus: "succeeded", Command: "/bin/sh -c true", ContractResult: "passed",
	})
	if got != certStatusPending {
		t.Errorf("CertificationStatus = %q; want pending", got)
	}
	if cert, err := store.GetCertifiedToolImageRecord("sha256:ph"); err == nil {
		t.Fatalf("placeholder L5-a certified the tool with status %q; want no certification", cert.PromotionStatus)
	}
	entries, err := store.ListToolFunctionCatalogEntries(index.PromotionActive)
	if err != nil {
		t.Fatalf("ListToolFunctionCatalogEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ACTIVE catalog entries = %d; want 0", len(entries))
	}
	// The record itself is still stored and still closes out the request.
	recs, err := store.ListToolCheckRecordsByImageDigest("sha256:ph")
	if err != nil || len(recs) != 1 {
		t.Errorf("stored check records = %d (err %v); want 1", len(recs), err)
	}
	vr, err := store.GetValidationRequestRecord("vr-ph")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if vr.ValidationStatus != index.ValidationSucceeded {
		t.Errorf("ValidationRequest status = %q; want Succeeded", vr.ValidationStatus)
	}
}

// TestSubmitCheckRecord_NonTerminalL5A_DoesNotCertifyAheadOfL5B verifies an
// evidence-bearing but non-terminal L5-a waits for L5-b.
func TestSubmitCheckRecord_NonTerminalL5A_DoesNotCertifyAheadOfL5B(t *testing.T) {
	ts, store := newServerWithRealCert(t)
	seedRegisteredImage(t, store, "cas-nt", "sha256:nt")

	got := postCheckRecord(t, ts, &catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-nt", ImageDigest: "sha256:nt", ToolName: "bwa", Version: "1.0",
		Stage: "L5A", Terminal: false, ValidationStatus: "succeeded", ValidationHash: "vh-nt",
		AllOutputsPresent: true, ContractResult: "passed",
	})
	if got != certStatusPending {
		t.Errorf("CertificationStatus = %q; want pending", got)
	}
	if _, err := store.GetCertifiedToolImageRecord("sha256:nt"); err == nil {
		t.Fatal("non-terminal L5-a certified the tool ahead of L5-b")
	}
}

// TestSubmitCheckRecord_TerminalEvidenceL5A_Certifies verifies the admissible
// shape still certifies through REST.
func TestSubmitCheckRecord_TerminalEvidenceL5A_Certifies(t *testing.T) {
	ts, store := newServerWithRealCert(t)
	seedRegisteredImage(t, store, "cas-ok", "sha256:ok")

	got := postCheckRecord(t, ts, &catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-ok", ImageDigest: "sha256:ok", ToolName: "bwa", Version: "1.0",
		Stage: "L5A", Terminal: true, ValidationStatus: "succeeded", ValidationHash: "vh-ok",
		AllOutputsPresent: true, ContractResult: "passed",
	})
	if got != "certified" {
		t.Errorf("CertificationStatus = %q; want certified", got)
	}
	cert, err := store.GetCertifiedToolImageRecord("sha256:ok")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.PromotionStatus != index.PromotionActive {
		t.Errorf("PromotionStatus = %q; want active", cert.PromotionStatus)
	}
}
