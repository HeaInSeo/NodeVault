package catalogrest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
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

// TestSubmitCheckRecord_ResultArrivesBeforeQueuedAck_ClosesOutFromEnqueuePending
// is the end-to-end guard for the EnqueuePending race: NodeSentinel can
// execute a job and submit its terminal result before NodeVault's own
// postBuildRegistration has processed the enqueue response and driven the
// record to Queued. The record here is deliberately left at EnqueuePending
// (the CreateValidationRequestRecord default) rather than seeded to Queued.
func TestSubmitCheckRecord_ResultArrivesBeforeQueuedAck_ClosesOutFromEnqueuePending(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})
	if err := store.CreateValidationRequestRecord(index.ValidationRequestRecord{
		ValidationRequestID: "vr-race", ImageDigest: "sha256:aaa",
	}); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}

	body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-race", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
		ValidationRequestID: "vr-race", SentinelJobID: "job-race", Stage: "L5B", Terminal: true,
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	rec, err := store.GetValidationRequestRecord("vr-race")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationSucceeded {
		t.Errorf("ValidationStatus = %q, want Succeeded (must not get stuck at EnqueuePending)", rec.ValidationStatus)
	}

	// The late enqueue ACK's own Queued transition must not regress this.
	lateAckErr := store.TransitionValidationRequest("vr-race", index.ValidationQueued, nil)
	if lateAckErr == nil {
		t.Error("expected the late enqueue ACK's Queued transition to be rejected")
	}
	rec, err = store.GetValidationRequestRecord("vr-race")
	if err != nil {
		t.Fatalf("GetValidationRequestRecord: %v", err)
	}
	if rec.ValidationStatus != index.ValidationSucceeded {
		t.Errorf("ValidationStatus after late ACK = %q, want unchanged Succeeded", rec.ValidationStatus)
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
		PolicyResult: "passed",
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

// TestSubmitCheckRecord_DuplicateCheckID_DifferentContent_Rejected409 is the
// converse of the idempotent-redelivery test above: the same CheckID
// resubmitted with materially different content (a different
// ValidationStatus here) must not be silently accepted as "already
// recorded" — that would hide whichever content lost the race. The original
// (first-write-wins) record must remain exactly as stored.
func TestSubmitCheckRecord_DuplicateCheckID_DifferentContent_Rejected409(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	first, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-conflict", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
	})
	firstResp := doPost(t, ts, ts.URL+"/v1/validation/check-records", first)
	defer func() { _ = firstResp.Body.Close() }()
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("first submission status: got %d want 200", firstResp.StatusCode)
	}

	second, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
		CheckID: "chk-conflict", ImageDigest: "sha256:aaa", ValidationStatus: "failed",
		FailureReason: "different outcome for the same check_id",
	})
	secondResp := doPost(t, ts, ts.URL+"/v1/validation/check-records", second)
	defer func() { _ = secondResp.Body.Close() }()
	if secondResp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting resubmission status: got %d want 409", secondResp.StatusCode)
	}

	stored, err := store.GetToolCheckRecordByID("chk-conflict")
	if err != nil {
		t.Fatalf("GetToolCheckRecordByID: %v", err)
	}
	if stored.ValidationStatus != "succeeded" {
		t.Errorf("stored ValidationStatus = %q, want the original %q (rejected conflict must not overwrite it)",
			stored.ValidationStatus, "succeeded")
	}
}

// TestSubmitCheckRecord_ConcurrentConflictingSubmissions_ExactlyOneWins
// exercises the same conflict detection under real concurrency: N goroutines
// submit the same CheckID with N distinct contents at once. Exactly one must
// be accepted (200) and the rest rejected (409) — never two accepted, never
// zero.
func TestSubmitCheckRecord_ConcurrentConflictingSubmissions_ExactlyOneWins(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	const workers = 8
	var wg sync.WaitGroup
	statusCodes := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(catalogrest.SubmitCheckRecordRequest{
				CheckID: "chk-race", ImageDigest: "sha256:aaa", ValidationStatus: "succeeded",
				Command: fmt.Sprintf("distinct-content-%d", i), // makes each submission's fingerprint unique
			})
			resp := doPost(t, ts, ts.URL+"/v1/validation/check-records", body)
			statusCodes[i] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, code := range statusCodes {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Errorf("unexpected status code %d", code)
		}
	}
	if okCount != 1 {
		t.Errorf("200 OK responses = %d, want exactly 1", okCount)
	}
	if conflictCount != workers-1 {
		t.Errorf("409 Conflict responses = %d, want %d", conflictCount, workers-1)
	}

	records, err := store.ListToolCheckRecordsByImageDigest("sha256:aaa")
	if err != nil {
		t.Fatalf("ListToolCheckRecordsByImageDigest: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("stored records = %d, want exactly 1 (only the winner)", len(records))
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

// TestSubmitScanRecord_InvalidPolicyResult_Rejected guards against a typo or
// unexpected policy_result value being silently treated as a passing scan —
// succeeded is derived as `PolicyResult != "blocked"`, so any unrecognized
// value would otherwise close a Terminal request out as Succeeded.
func TestSubmitScanRecord_InvalidPolicyResult_Rejected(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID: "scan-typo", ImageDigest: "sha256:aaa", PolicyResult: "gate_critical",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	if _, err := store.GetToolScanRecordByID("scan-typo"); err == nil {
		t.Error("record must not be stored when policy_result is invalid")
	}
}

// TestSubmitScanRecord_EmptyPolicyResult_Rejected verifies that a NEW
// submission must state a policy_result explicitly — NodeSentinel's own
// submission paths always set one (see pkg/worker/l5b.go), so silently
// accepting an empty value here would only ever mask a caller bug. This is
// deliberately stricter than reading already-stored pre-validation rows,
// which may still have PolicyResult == "" on disk — see
// terminalSucceededForPolicyResult's doc comment in server.go.
func TestSubmitScanRecord_EmptyPolicyResult_Rejected(t *testing.T) {
	ts, store := newServerWithCert(t, &fakeCertSvc{})

	body, _ := json.Marshal(catalogrest.SubmitScanRecordRequest{
		ScanID: "scan-empty-policy", ImageDigest: "sha256:aaa",
	})
	resp := doPost(t, ts, ts.URL+"/v1/validation/scan-records", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	if _, err := store.GetToolScanRecordByID("scan-empty-policy"); err == nil {
		t.Error("record must not be stored when policy_result is empty")
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
