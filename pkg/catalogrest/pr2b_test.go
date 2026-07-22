package catalogrest_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// seedQueuedValidationRequest creates a ValidationRequestRecord and drives it
// to Queued — the status postBuildRegistration leaves it in after a
// successful NodeSentinel enqueue (see pkg/build/service.go). Tests use this
// to set up the state a validation result submission expects to correlate
// against.
func seedQueuedValidationRequest(t *testing.T, store *index.Store, id, imageDigest string) {
	t.Helper()
	if err := store.CreateValidationRequestRecord(index.ValidationRequestRecord{
		ValidationRequestID: id,
		ImageDigest:         imageDigest,
	}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}
	if err := store.TransitionValidationRequest(id, index.ValidationQueued, nil); err != nil {
		t.Fatalf("TransitionValidationRequest to Queued: %v", err)
	}
}

// ── missing/orphan validation_request_id: fail-open ──────────────────────────

func TestSubmitCheckRecord_MissingValidationRequestID_StillStoredFailOpen(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-missing-id", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if _, err := store.GetToolCheckRecordByID("chk-missing-id"); err != nil {
		t.Errorf("expected record to be stored despite missing validation_request_id: %v", err)
	}
}

func TestSubmitCheckRecord_UnknownValidationRequestID_OrphanFailOpen(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-orphan", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
		ValidationRequestID: "vr-does-not-exist",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200 (orphan id must not block storage)", resp.StatusCode)
	}
	if _, err := store.GetToolCheckRecordByID("chk-orphan"); err != nil {
		t.Errorf("expected record to be stored despite an orphan validation_request_id: %v", err)
	}
}

// ── digest mismatch: rejected, not stored ────────────────────────────────────

func TestSubmitCheckRecord_DigestMismatch_Rejected(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-1", "sha256:expected")

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-mismatch", ImageDigest: "sha256:different", ValidationStatus: "succeeded",
		ValidationRequestID: "vr-1",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d want 409", resp.StatusCode)
	}
	if _, err := store.GetToolCheckRecordByID("chk-mismatch"); err == nil {
		t.Error("record must not be stored when image_digest doesn't match the validation_request_id's recorded request")
	}
	rec, err := store.GetValidationRequestRecord("vr-1")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationQueued {
		t.Errorf("ValidationStatus = %q, want unchanged Queued after a rejected mismatch", rec.ValidationStatus)
	}
}

func TestSubmitScanRecord_DigestMismatch_Rejected(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-scan-1", "sha256:expected")

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID: "scan-mismatch", ImageDigest: "sha256:different", ValidationRequestID: "vr-scan-1",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d want 409", resp.StatusCode)
	}
	if _, err := store.GetToolScanRecordByID("scan-mismatch"); err == nil {
		t.Error("record must not be stored on a digest mismatch")
	}
}

// ── idempotent redelivery: duplicate CheckID/ScanID is not an error ─────────

func TestSubmitCheckRecord_DuplicateCheckID_IdempotentNotError(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-dup", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
	})

	first := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first submission status: got %d want 200", first.StatusCode)
	}

	second := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("redelivery status: got %d want 200 (duplicate must be idempotent, not an error)", second.StatusCode)
	}
	var result catalogrest.SubmitRecordResponse
	if err := json.NewDecoder(second.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.CertificationStatus != "already_recorded" {
		t.Errorf("CertificationStatus = %q, want already_recorded", result.CertificationStatus)
	}

	records, err := store.ListToolCheckRecordsByImageDigest("sha256:aaa")
	if err != nil {
		t.Fatalf("ListToolCheckRecordsByImageDigest: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("stored records = %d, want exactly 1 (redelivery must not create a duplicate)", len(records))
	}
}

// ── correlation-driven status transitions ────────────────────────────────────

func TestSubmitCheckRecord_NonTerminal_PromotesQueuedToRunningOnly(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-running", "sha256:aaa")

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-l5a", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
		ValidationRequestID: "vr-running", Stage: "L5A", Terminal: false,
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	rec, err := store.GetValidationRequestRecord("vr-running")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationRunning {
		t.Errorf("ValidationStatus = %q, want Running (non-terminal record must not close out the request)", rec.ValidationStatus)
	}
}

func TestSubmitCheckRecord_Terminal_Succeeded_ClosesOutValidationRequest(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-succeed", "sha256:aaa")

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-l3-fail-path-succeed", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
		ValidationRequestID: "vr-succeed", SentinelJobID: "job-xyz", Stage: "L5B", Terminal: true,
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	rec, err := store.GetValidationRequestRecord("vr-succeed")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationSucceeded {
		t.Errorf("ValidationStatus = %q, want Succeeded", rec.ValidationStatus)
	}
	if rec.SentinelJobID != "job-xyz" {
		t.Errorf("SentinelJobID = %q, want job-xyz", rec.SentinelJobID)
	}
	if rec.CompletedAt.IsZero() {
		t.Error("CompletedAt was not set")
	}
}

// TestSubmitCheckRecord_Terminal_L3Failure_ClosesOutAsFailed is PR2-B's core
// end-to-end guard: an L3/L4 failure submitted through this same endpoint
// (see pkg/worker's reportTerminalFailure) must close the correlated
// ValidationRequestRecord out to Failed — before PR2-B nothing ever reported
// this and the record stayed stuck at Queued forever.
func TestSubmitCheckRecord_Terminal_L3Failure_ClosesOutAsFailed(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-l3-fail", "sha256:aaa")

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "l3-fail-1", ImageDigest: "sha256:aaa", ValidationStatus: "infra_failed",
		ValidationRequestID: "vr-l3-fail", Stage: "L3", Terminal: true,
		FailureKind: "infrastructure", FailureReason: "admission webhook rejected",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	rec, err := store.GetValidationRequestRecord("vr-l3-fail")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationFailed {
		t.Errorf("ValidationStatus = %q, want Failed", rec.ValidationStatus)
	}
	if rec.FailureReason != "admission webhook rejected" {
		t.Errorf("FailureReason = %q, want the submitted reason", rec.FailureReason)
	}
}

func TestSubmitScanRecord_Terminal_Blocked_ClosesOutAsFailed(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-scan-blocked", "sha256:aaa")

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID: "scan-blocked", ImageDigest: "sha256:aaa",
		ValidationRequestID: "vr-scan-blocked", Stage: "L5B", Terminal: true,
		PolicyMode: "gate_critical", PolicyResult: "blocked", CriticalCount: 3,
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	rec, err := store.GetValidationRequestRecord("vr-scan-blocked")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationFailed {
		t.Errorf("ValidationStatus = %q, want Failed (a blocked policy result must fail the overall request)", rec.ValidationStatus)
	}
}

func TestSubmitScanRecord_Terminal_Warning_ClosesOutAsSucceeded(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	seedQueuedValidationRequest(t, store, "vr-scan-warning", "sha256:aaa")

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID: "scan-warning", ImageDigest: "sha256:aaa",
		ValidationRequestID: "vr-scan-warning", Stage: "L5B", Terminal: true,
		PolicyMode: "record_only", PolicyResult: "warning", HighCount: 2,
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	rec, err := store.GetValidationRequestRecord("vr-scan-warning")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationSucceeded {
		t.Errorf("ValidationStatus = %q, want Succeeded (a warning must not fail the overall request)", rec.ValidationStatus)
	}
}
