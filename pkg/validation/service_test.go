package validation_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/validation"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ── fake certificationService ──────────────────────────────────────────────────

type fakeCertSvc struct {
	checkErr error
	scanErr  error
	called   bool
}

//nolint:gocritic // hugeParam: interface method signatures are fixed by certificationService interface.
func (f *fakeCertSvc) EvaluateAfterCheck(_ index.ToolCheckRecord) error {
	f.called = true
	return f.checkErr
}

//nolint:gocritic // hugeParam: interface method signatures are fixed by certificationService interface.
func (f *fakeCertSvc) EvaluateAfterScan(_ index.ToolScanRecord) error {
	f.called = true
	return f.scanErr
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return store
}

func grpcCode(err error) codes.Code {
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return codes.Unknown
}

// ── SubmitToolCheckRecord tests ───────────────────────────────────────────────

// TestSubmitToolCheckRecord_HappyPath verifies a successful submission.
func TestSubmitToolCheckRecord_HappyPath(t *testing.T) {
	store := newStore(t)
	certSvc := &fakeCertSvc{}
	svc := validation.New(store, certSvc)

	req := &nfv1.ToolCheckRecordRequest{
		CheckId:          "chk-1",
		ImageDigest:      "sha256:aaa",
		ToolName:         "bwa",
		Version:          "1.0",
		ValidationStatus: "succeeded",
	}
	resp, err := svc.SubmitToolCheckRecord(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitToolCheckRecord: %v", err)
	}
	if resp.RecordId != "chk-1" {
		t.Errorf("RecordId: got %q want chk-1", resp.RecordId)
	}
	if resp.CertificationStatus != "certified" {
		t.Errorf("CertificationStatus: got %q want certified", resp.CertificationStatus)
	}
	if !certSvc.called {
		t.Error("expected certSvc.EvaluateAfterCheck to be called")
	}
}

// TestSubmitToolCheckRecord_CertFailed verifies WARN-4: certSvc error sets certStatus="failed".
func TestSubmitToolCheckRecord_CertFailed(t *testing.T) {
	store := newStore(t)
	certSvc := &fakeCertSvc{checkErr: errors.New("cert boom")}
	svc := validation.New(store, certSvc)

	req := &nfv1.ToolCheckRecordRequest{
		CheckId:          "chk-2",
		ImageDigest:      "sha256:bbb",
		ToolName:         "bwa",
		Version:          "1.0",
		ValidationStatus: "succeeded",
	}
	resp, err := svc.SubmitToolCheckRecord(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitToolCheckRecord: %v", err)
	}
	if resp.CertificationStatus != "failed" {
		t.Errorf("CertificationStatus: got %q want failed", resp.CertificationStatus)
	}
}

// TestSubmitToolCheckRecord_NegativeCheckedAt verifies WARN-5: negative checked_at is rejected.
func TestSubmitToolCheckRecord_NegativeCheckedAt(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	req := &nfv1.ToolCheckRecordRequest{
		CheckId:     "chk-3",
		ImageDigest: "sha256:ccc",
		CheckedAt:   -1,
	}
	_, err := svc.SubmitToolCheckRecord(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for negative checked_at")
	}
	if grpcCode(err) != codes.InvalidArgument {
		t.Errorf("code: got %v want InvalidArgument", grpcCode(err))
	}
}

// TestSubmitToolCheckRecord_TooLargeCheckedAt verifies WARN-5: out-of-range checked_at is rejected.
func TestSubmitToolCheckRecord_TooLargeCheckedAt(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	req := &nfv1.ToolCheckRecordRequest{
		CheckId:     "chk-4",
		ImageDigest: "sha256:ddd",
		CheckedAt:   253402300800000, // one ms past max
	}
	_, err := svc.SubmitToolCheckRecord(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for too-large checked_at")
	}
	if grpcCode(err) != codes.InvalidArgument {
		t.Errorf("code: got %v want InvalidArgument", grpcCode(err))
	}
}

// TestSubmitToolCheckRecord_MissingCheckID verifies that missing check_id returns InvalidArgument.
func TestSubmitToolCheckRecord_MissingCheckID(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	req := &nfv1.ToolCheckRecordRequest{
		CheckId:     "",
		ImageDigest: "sha256:eee",
	}
	_, err := svc.SubmitToolCheckRecord(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing check_id")
	}
	if grpcCode(err) != codes.InvalidArgument {
		t.Errorf("code: got %v want InvalidArgument", grpcCode(err))
	}
}

// TestSubmitToolCheckRecord_StoreError verifies that a duplicate CheckID returns codes.Internal.
func TestSubmitToolCheckRecord_StoreError(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	req := &nfv1.ToolCheckRecordRequest{
		CheckId:     "chk-dup",
		ImageDigest: "sha256:fff",
	}
	// First call succeeds.
	if _, err := svc.SubmitToolCheckRecord(context.Background(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call with same CheckID should fail (duplicate).
	_, err := svc.SubmitToolCheckRecord(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for duplicate CheckID")
	}
	if grpcCode(err) != codes.Internal {
		t.Errorf("code: got %v want Internal", grpcCode(err))
	}
}

// ── SubmitToolScanRecord tests ────────────────────────────────────────────────

// TestSubmitToolScanRecord_HappyPath verifies a successful scan submission.
func TestSubmitToolScanRecord_HappyPath(t *testing.T) {
	store := newStore(t)
	certSvc := &fakeCertSvc{}
	svc := validation.New(store, certSvc)

	req := &nfv1.ToolScanRecordRequest{
		ScanId:      "scan-1",
		ImageDigest: "sha256:aaa",
		Scanner:     "trivy",
		PolicyMode:  "gate_critical",
	}
	resp, err := svc.SubmitToolScanRecord(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitToolScanRecord: %v", err)
	}
	if resp.RecordId != "scan-1" {
		t.Errorf("RecordId: got %q want scan-1", resp.RecordId)
	}
	if !certSvc.called {
		t.Error("expected certSvc.EvaluateAfterScan to be called")
	}
}

// TestSubmitToolScanRecord_NegativeScannedAt verifies WARN-5 for scan: negative scanned_at rejected.
func TestSubmitToolScanRecord_NegativeScannedAt(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	req := &nfv1.ToolScanRecordRequest{
		ScanId:      "scan-2",
		ImageDigest: "sha256:bbb",
		ScannedAt:   -1,
	}
	_, err := svc.SubmitToolScanRecord(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for negative scanned_at")
	}
	if grpcCode(err) != codes.InvalidArgument {
		t.Errorf("code: got %v want InvalidArgument", grpcCode(err))
	}
}

// ── ListCertifiedTools tests ──────────────────────────────────────────────────

// TestListCertifiedTools_EmptyStatus verifies empty promotion_status defaults to active.
func TestListCertifiedTools_EmptyStatus(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	// Seed an active entry.
	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash:         "cas-1",
		ToolName:        "bwa",
		Version:         "1.0",
		StableRef:       "bwa@1.0",
		PromotionStatus: index.PromotionActive,
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}

	resp, err := svc.ListCertifiedTools(context.Background(), &nfv1.ListCertifiedToolsRequest{})
	if err != nil {
		t.Fatalf("ListCertifiedTools: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(resp.Tools))
	}
}

// TestListCertifiedTools_ValidActiveStatus verifies explicit "active" status filter.
func TestListCertifiedTools_ValidActiveStatus(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash:         "cas-2",
		ToolName:        "samtools",
		Version:         "1.17",
		PromotionStatus: index.PromotionActive,
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}

	resp, err := svc.ListCertifiedTools(context.Background(), &nfv1.ListCertifiedToolsRequest{
		PromotionStatus: "active",
	})
	if err != nil {
		t.Fatalf("ListCertifiedTools: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(resp.Tools))
	}
	if resp.Tools[0].PromotionStatus != "active" {
		t.Errorf("PromotionStatus: got %q want active", resp.Tools[0].PromotionStatus)
	}
}

// TestListCertifiedTools_InvalidStatus verifies WARN-6: invalid promotion_status is rejected.
func TestListCertifiedTools_InvalidStatus(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	_, err := svc.ListCertifiedTools(context.Background(), &nfv1.ListCertifiedToolsRequest{
		PromotionStatus: "INVALID",
	})
	if err == nil {
		t.Fatal("expected error for invalid promotion_status")
	}
	if grpcCode(err) != codes.InvalidArgument {
		t.Errorf("code: got %v want InvalidArgument", grpcCode(err))
	}
}

// TestListCertifiedTools_SupersededStatus verifies "superseded" is a valid status.
func TestListCertifiedTools_SupersededStatus(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash:         "cas-3",
		ToolName:        "hisat2",
		Version:         "2.1.0",
		PromotionStatus: index.PromotionSuperseded,
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}

	resp, err := svc.ListCertifiedTools(context.Background(), &nfv1.ListCertifiedToolsRequest{
		PromotionStatus: "superseded",
	})
	if err != nil {
		t.Fatalf("ListCertifiedTools: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(resp.Tools))
	}
}

// TestListCertifiedTools_RetractedStatus verifies "retracted" is a valid status.
func TestListCertifiedTools_RetractedStatus(t *testing.T) {
	store := newStore(t)
	svc := validation.New(store, nil)

	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash:         "cas-4",
		ToolName:        "star",
		Version:         "2.7.11",
		PromotionStatus: index.PromotionRetracted,
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}

	resp, err := svc.ListCertifiedTools(context.Background(), &nfv1.ListCertifiedToolsRequest{
		PromotionStatus: "retracted",
	})
	if err != nil {
		t.Fatalf("ListCertifiedTools: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(resp.Tools))
	}
	if resp.Tools[0].PromotionStatus != "retracted" {
		t.Errorf("PromotionStatus: got %q want retracted", resp.Tools[0].PromotionStatus)
	}
}
