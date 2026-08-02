package certification_test

import (
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

func newStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return store
}

func newCheckRecord(checkID, imageDigest, toolName, version, status string) index.ToolCheckRecord {
	return index.ToolCheckRecord{
		CheckID:          checkID,
		ImageDigest:      imageDigest,
		ToolName:         toolName,
		Version:          version,
		ValidationStatus: status,
		ValidationHash:   "hash-" + checkID,
		CheckedAt:        time.Now().UTC(),
	}
}

// TestEvaluateAfterCheck_HappyPath verifies that a succeeded check creates a
// CertifiedToolImageRecord with PromotionActive.
func TestEvaluateAfterCheck_HappyPath(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	check := newCheckRecord("chk-1", "sha256:aaa", "bwa", "1.0", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	if err := svc.EvaluateAfterCheck(check); err != nil {
		t.Fatalf("EvaluateAfterCheck: %v", err)
	}

	cert, err := store.GetCertifiedToolImageRecord("sha256:aaa")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.PromotionStatus != index.PromotionActive {
		t.Errorf("PromotionStatus: got %q want %q", cert.PromotionStatus, index.PromotionActive)
	}
	if cert.ToolName != "bwa" {
		t.Errorf("ToolName: got %q want bwa", cert.ToolName)
	}
	if cert.Version != "1.0" {
		t.Errorf("Version: got %q want 1.0", cert.Version)
	}
	if cert.CheckID != "chk-1" {
		t.Errorf("CheckID: got %q want chk-1", cert.CheckID)
	}
}

// TestEvaluateAfterCheck_WithCasHash verifies that when a matching index Entry
// exists, the CasHash is populated and a ToolFunctionCatalogEntry is written.
func TestEvaluateAfterCheck_WithCasHash(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	// Pre-seed an Entry with matching StableRef.
	entry := index.Entry{
		CasHash:         "cas-abc123",
		ArtifactKind:    index.KindTool,
		StableRef:       "bwa@1.0",
		ToolName:        "bwa",
		Version:         "1.0",
		ImageDigest:     "sha256:bbb",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}
	if err := store.Append(entry); err != nil {
		t.Fatalf("store.Append: %v", err)
	}

	check := newCheckRecord("chk-2", "sha256:bbb", "bwa", "1.0", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	if err := svc.EvaluateAfterCheck(check); err != nil {
		t.Fatalf("EvaluateAfterCheck: %v", err)
	}

	// Verify catalog entry was created.
	catEntries, err := store.ListToolFunctionCatalogEntries(index.PromotionActive)
	if err != nil {
		t.Fatalf("ListToolFunctionCatalogEntries: %v", err)
	}
	if len(catEntries) != 1 {
		t.Fatalf("expected 1 catalog entry, got %d", len(catEntries))
	}
	if catEntries[0].CasHash != "cas-abc123" {
		t.Errorf("CasHash: got %q want cas-abc123", catEntries[0].CasHash)
	}
	if catEntries[0].StableRef != "bwa@1.0" {
		t.Errorf("StableRef: got %q want bwa@1.0", catEntries[0].StableRef)
	}
}

// TestEvaluateAfterCheck_NotSucceeded verifies that a check with status != "succeeded"
// returns nil without calling certify.
func TestEvaluateAfterCheck_NotSucceeded(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	check := newCheckRecord("chk-3", "sha256:ccc", "bwa", "1.0", "app_failed")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	if err := svc.EvaluateAfterCheck(check); err != nil {
		t.Errorf("EvaluateAfterCheck with app_failed should return nil, got: %v", err)
	}

	// No certification record should be written.
	_, err := store.GetCertifiedToolImageRecord("sha256:ccc")
	if err == nil {
		t.Error("expected no CertifiedToolImageRecord to be written for non-succeeded check")
	}
}

// TestEvaluateAfterCheck_EmptyToolName verifies WARN-3: empty ToolName returns error.
func TestEvaluateAfterCheck_EmptyToolName(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	check := newCheckRecord("chk-4", "sha256:ddd", "", "1.0", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	err := svc.EvaluateAfterCheck(check)
	if err == nil {
		t.Error("expected error for empty ToolName, got nil")
	}
}

// TestEvaluateAfterCheck_EmptyVersion verifies WARN-3: empty Version returns error.
func TestEvaluateAfterCheck_EmptyVersion(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	check := newCheckRecord("chk-5", "sha256:eee", "bwa", "", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	err := svc.EvaluateAfterCheck(check)
	if err == nil {
		t.Error("expected error for empty Version, got nil")
	}
}

// TestEvaluateAfterCheck_EmptyImageDigest verifies that when ImageDigest is empty,
// UpsertCertifiedToolImageRecord fails and certify returns an error.
func TestEvaluateAfterCheck_EmptyImageDigest(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	// Build a record with ToolName/Version set but empty ImageDigest.
	// The store will reject UpsertCertifiedToolImageRecord with empty ImageDigest.
	check := index.ToolCheckRecord{
		CheckID:          "chk-6",
		ImageDigest:      "", // empty — store will reject
		ToolName:         "bwa",
		Version:          "1.0",
		ValidationStatus: "succeeded",
		CheckedAt:        time.Now().UTC(),
	}
	// We cannot append this to the store (CheckID non-empty, but we call certify directly
	// via EvaluateAfterCheck which will still call certify with the empty ImageDigest).
	// Append will succeed (store only checks CheckID non-empty).
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	err := svc.EvaluateAfterCheck(check)
	if err == nil {
		t.Error("expected error when ImageDigest is empty (UpsertCertifiedToolImageRecord should fail)")
	}
}

// TestEvaluateAfterCheck_MultipleImagesSameStableRef is the gap #19 regression:
// when two different images are registered under the same tool_name@version
// (stableRef is 1:N per the constitution), certifying the SECOND image must
// bind the catalog entry to the SECOND registration's CasHash — not the oldest.
// The pre-fix code used ListByStableRef(...)[0] (oldest registration), so this
// test fails before the ImageDigest-based lookup and passes after it.
func TestEvaluateAfterCheck_MultipleImagesSameStableRef(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	// Oldest registration under bwa@1.0 (image 1).
	if err := store.Append(index.Entry{
		CasHash:         "cas-img1-old",
		ArtifactKind:    index.KindTool,
		StableRef:       "bwa@1.0",
		ToolName:        "bwa",
		Version:         "1.0",
		ImageDigest:     "sha256:img1",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}); err != nil {
		t.Fatalf("store.Append img1: %v", err)
	}
	// Second registration under the SAME stableRef, different image (image 2).
	if err := store.Append(index.Entry{
		CasHash:         "cas-img2-new",
		ArtifactKind:    index.KindTool,
		StableRef:       "bwa@1.0",
		ToolName:        "bwa",
		Version:         "1.0",
		ImageDigest:     "sha256:img2",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}); err != nil {
		t.Fatalf("store.Append img2: %v", err)
	}

	// Certify the SECOND image.
	check := newCheckRecord("chk-multi", "sha256:img2", "bwa", "1.0", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	if err := svc.EvaluateAfterCheck(check); err != nil {
		t.Fatalf("EvaluateAfterCheck: %v", err)
	}

	cert, err := store.GetCertifiedToolImageRecord("sha256:img2")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.CasHash != "cas-img2-new" {
		t.Errorf("cert.CasHash: got %q want cas-img2-new (bound to the wrong image — gap #19)", cert.CasHash)
	}

	catEntries, err := store.ListToolFunctionCatalogEntries(index.PromotionActive)
	if err != nil {
		t.Fatalf("ListToolFunctionCatalogEntries: %v", err)
	}
	if len(catEntries) != 1 {
		t.Fatalf("expected 1 catalog entry, got %d", len(catEntries))
	}
	if catEntries[0].CasHash != "cas-img2-new" {
		t.Errorf("catalog CasHash: got %q want cas-img2-new (the certified image), not the oldest registration", catEntries[0].CasHash)
	}
}

// TestEvaluateAfterScan_ExistingCheck verifies that when a scan arrives and a
// succeeded check already exists, certification is triggered.
func TestEvaluateAfterScan_ExistingCheck(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	check := newCheckRecord("chk-7", "sha256:fff", "samtools", "1.17", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	scan := index.ToolScanRecord{
		ScanID:       "scan-1",
		ImageDigest:  "sha256:fff",
		PolicyMode:   "gate_critical",
		PolicyResult: "pass",
		ScannedAt:    time.Now().UTC(),
	}
	if err := store.AppendToolScanRecord(scan); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}

	if err := svc.EvaluateAfterScan(scan); err != nil {
		t.Fatalf("EvaluateAfterScan: %v", err)
	}

	cert, err := store.GetCertifiedToolImageRecord("sha256:fff")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.ScanID != "scan-1" {
		t.Errorf("ScanID: got %q want scan-1", cert.ScanID)
	}
	if cert.PromotionStatus != index.PromotionActive {
		t.Errorf("PromotionStatus: got %q want active", cert.PromotionStatus)
	}
}

// TestEvaluateAfterScan_NoCheck verifies that when a scan arrives but no
// succeeded check exists, no error is returned (deferred certification).
func TestEvaluateAfterScan_NoCheck(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	scan := index.ToolScanRecord{
		ScanID:      "scan-2",
		ImageDigest: "sha256:ggg",
		ScannedAt:   time.Now().UTC(),
	}
	if err := store.AppendToolScanRecord(scan); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}

	if err := svc.EvaluateAfterScan(scan); err != nil {
		t.Errorf("EvaluateAfterScan with no check should return nil, got: %v", err)
	}

	// No certification record should be written.
	_, err := store.GetCertifiedToolImageRecord("sha256:ggg")
	if err == nil {
		t.Error("expected no CertifiedToolImageRecord when no succeeded check exists")
	}
}

// TestEvaluateAfterScan_PolicyBlocked verifies that a blocked scan results in
// PromotionRetracted.
func TestEvaluateAfterScan_PolicyBlocked(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)

	check := newCheckRecord("chk-8", "sha256:hhh", "hisat2", "2.2.1", "succeeded")
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}

	scan := index.ToolScanRecord{
		ScanID:       "scan-3",
		ImageDigest:  "sha256:hhh",
		PolicyMode:   "gate_critical",
		PolicyResult: "blocked",
		ScannedAt:    time.Now().UTC(),
	}
	if err := store.AppendToolScanRecord(scan); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}

	if err := svc.EvaluateAfterScan(scan); err != nil {
		t.Fatalf("EvaluateAfterScan: %v", err)
	}

	cert, err := store.GetCertifiedToolImageRecord("sha256:hhh")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.PromotionStatus != index.PromotionRetracted {
		t.Errorf("PromotionStatus: got %q want retracted", cert.PromotionStatus)
	}
}
