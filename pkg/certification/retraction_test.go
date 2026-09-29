package certification_test

import (
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// seedActiveCertification stores an ACTIVE certification and catalog entry
// as a pre-#117 build would have left them, driven by checkID/scanID.
func seedActiveCertification(t *testing.T, store *index.Store, imageDigest, casHash, checkID, scanID string) {
	t.Helper()
	if err := store.UpsertCertifiedToolImageRecord(index.CertifiedToolImageRecord{
		ImageDigest: imageDigest, ToolName: "bwa", Version: "1.0", CasHash: casHash,
		PromotionStatus: index.PromotionActive, CheckID: checkID, ScanID: scanID,
	}); err != nil {
		t.Fatalf("UpsertCertifiedToolImageRecord: %v", err)
	}
	if err := store.UpsertToolFunctionCatalogEntry(index.ToolFunctionCatalogEntry{
		CasHash: casHash, ToolName: "bwa", Version: "1.0", ImageDigest: imageDigest,
		PromotionStatus: index.PromotionActive,
	}); err != nil {
		t.Fatalf("UpsertToolFunctionCatalogEntry: %v", err)
	}
}

func assertPromotion(t *testing.T, store *index.Store, imageDigest, casHash string, want index.PromotionStatus) {
	t.Helper()
	cert, err := store.GetCertifiedToolImageRecord(imageDigest)
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord(%s): %v", imageDigest, err)
	}
	if cert.PromotionStatus != want {
		t.Errorf("cert %s status = %q; want %q", imageDigest, cert.PromotionStatus, want)
	}
	entries, err := store.ListToolFunctionCatalogEntries("")
	if err != nil {
		t.Fatalf("ListToolFunctionCatalogEntries: %v", err)
	}
	for i := range entries {
		if entries[i].CasHash == casHash {
			if entries[i].PromotionStatus != want {
				t.Errorf("catalog %s status = %q; want %q", casHash, entries[i].PromotionStatus, want)
			}
			return
		}
	}
	t.Errorf("catalog entry %s missing; retraction must not delete it", casHash)
}

// TestRetractUnprovenCertifications is the Codex P1 upgrade regression:
// ACTIVE certifications the current admission rule cannot re-prove from their
// stored driving records are retracted (not deleted) together with their
// catalog entries, while provable ones stay ACTIVE.
func TestRetractUnprovenCertifications(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)
	now := time.Now().UTC()

	// Placeholder L5-a that certified before #117.
	if err := store.AppendToolCheckRecord(placeholderL5A("chk-old-ph", "sha256:oldph", true)); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	seedActiveCertification(t, store, "sha256:oldph", "cas-oldph", "chk-old-ph", "")

	// Driving check record missing.
	seedActiveCertification(t, store, "sha256:nochk", "cas-nochk", "chk-missing", "")

	// Evidence-bearing but non-terminal L5-a promoted by a non-terminal scan.
	nonTerm := newCheckRecord("chk-nt", "sha256:nt", "bwa", "1.0", "succeeded")
	nonTerm.Terminal = false
	if err := store.AppendToolCheckRecord(nonTerm); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	if err := store.AppendToolScanRecord(index.ToolScanRecord{
		ScanID: "scan-nt", ImageDigest: "sha256:nt", Stage: "L5B", Terminal: false,
		PolicyResult: "passed", ScannedAt: now,
	}); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}
	seedActiveCertification(t, store, "sha256:nt", "cas-nt", "chk-nt", "scan-nt")

	// Provable: terminal evidence-bearing L5-a.
	if err := store.AppendToolCheckRecord(newCheckRecord("chk-good", "sha256:good", "bwa", "1.0", "succeeded")); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	seedActiveCertification(t, store, "sha256:good", "cas-good", "chk-good", "")

	// Provable: non-terminal evidence-bearing L5-a closed by a terminal L5-b.
	seq := newCheckRecord("chk-seq", "sha256:seq", "bwa", "1.0", "succeeded")
	seq.Terminal = false
	if err := store.AppendToolCheckRecord(seq); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	if err := store.AppendToolScanRecord(index.ToolScanRecord{
		ScanID: "scan-seq", ImageDigest: "sha256:seq", Stage: "L5B", Terminal: true,
		PolicyResult: "passed", ScannedAt: now,
	}); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}
	seedActiveCertification(t, store, "sha256:seq", "cas-seq", "chk-seq", "scan-seq")

	n, err := svc.RetractUnprovenCertifications()
	if err != nil {
		t.Fatalf("RetractUnprovenCertifications: %v", err)
	}
	if n != 3 {
		t.Errorf("retracted = %d; want 3", n)
	}
	assertPromotion(t, store, "sha256:oldph", "cas-oldph", index.PromotionRetracted)
	assertPromotion(t, store, "sha256:nochk", "cas-nochk", index.PromotionRetracted)
	assertPromotion(t, store, "sha256:nt", "cas-nt", index.PromotionRetracted)
	assertPromotion(t, store, "sha256:good", "cas-good", index.PromotionActive)
	assertPromotion(t, store, "sha256:seq", "cas-seq", index.PromotionActive)

	// Idempotent: a second pass retracts nothing more.
	if n, err := svc.RetractUnprovenCertifications(); err != nil || n != 0 {
		t.Errorf("second pass = (%d, %v); want (0, nil)", n, err)
	}
}

func activeCatalogEntry(t *testing.T, store *index.Store, casHash string) index.ToolFunctionCatalogEntry {
	t.Helper()
	entries, err := store.ListToolFunctionCatalogEntries(index.PromotionActive)
	if err != nil {
		t.Fatalf("ListToolFunctionCatalogEntries: %v", err)
	}
	for i := range entries {
		if entries[i].CasHash == casHash {
			return entries[i]
		}
	}
	t.Fatalf("catalog entry %s is not ACTIVE; a valid certification still backs it", casHash)
	return index.ToolFunctionCatalogEntry{}
}

func certStatus(t *testing.T, store *index.Store, imageDigest string) index.PromotionStatus {
	t.Helper()
	cert, err := store.GetCertifiedToolImageRecord(imageDigest)
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord(%s): %v", imageDigest, err)
	}
	return cert.PromotionStatus
}

// TestRetractUnprovenCertifications_SharedCasKeptByValidSurvivor is the Codex
// P2 regression (PR #118 thread 4138588064): two ACTIVE certifications share
// one CAS hash, one unprovable and one provable. The unprovable one is
// retracted, but the shared catalog entry stays ACTIVE. If the entry pointed
// at the retracted image it is rebuilt from the survivor.
func TestRetractUnprovenCertifications_SharedCasKeptByValidSurvivor(t *testing.T) {
	cases := []struct {
		name        string
		invalidLast bool // catalog entry was last written by the invalid certification
	}{
		{name: "catalog points at survivor", invalidLast: false},
		{name: "catalog points at retracted image", invalidLast: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			svc := certification.New(store)
			if err := store.AppendToolCheckRecord(placeholderL5A("chk-sh-ph", "sha256:shph", true)); err != nil {
				t.Fatalf("AppendToolCheckRecord: %v", err)
			}
			if err := store.AppendToolCheckRecord(newCheckRecord("chk-sh-ok", "sha256:shok", "bwa", "1.0", "succeeded")); err != nil {
				t.Fatalf("AppendToolCheckRecord: %v", err)
			}
			if tc.invalidLast {
				seedActiveCertification(t, store, "sha256:shok", "cas-shared", "chk-sh-ok", "")
				seedActiveCertification(t, store, "sha256:shph", "cas-shared", "chk-sh-ph", "")
			} else {
				seedActiveCertification(t, store, "sha256:shph", "cas-shared", "chk-sh-ph", "")
				seedActiveCertification(t, store, "sha256:shok", "cas-shared", "chk-sh-ok", "")
			}

			n, err := svc.RetractUnprovenCertifications()
			if err != nil {
				t.Fatalf("RetractUnprovenCertifications: %v", err)
			}
			if n != 1 {
				t.Errorf("retracted = %d; want 1", n)
			}
			if got := certStatus(t, store, "sha256:shph"); got != index.PromotionRetracted {
				t.Errorf("invalid cert status = %q; want retracted", got)
			}
			if got := certStatus(t, store, "sha256:shok"); got != index.PromotionActive {
				t.Errorf("valid cert status = %q; want active", got)
			}
			entry := activeCatalogEntry(t, store, "cas-shared")
			if entry.ImageDigest != "sha256:shok" {
				t.Errorf("catalog image = %q; want survivor sha256:shok", entry.ImageDigest)
			}
			if tc.invalidLast && entry.ValidationHash != "hash-chk-sh-ok" {
				t.Errorf("rebuilt catalog validation hash = %q; want survivor's hash-chk-sh-ok", entry.ValidationHash)
			}

			if n, err := svc.RetractUnprovenCertifications(); err != nil || n != 0 {
				t.Errorf("second pass = (%d, %v); want (0, nil)", n, err)
			}
			activeCatalogEntry(t, store, "cas-shared")
		})
	}
}

// TestRetractUnprovenCertifications_SharedCasAllInvalidRetracts: when every
// ACTIVE certification sharing a CAS hash is unprovable, the catalog entry is
// retracted.
func TestRetractUnprovenCertifications_SharedCasAllInvalidRetracts(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)
	if err := store.AppendToolCheckRecord(placeholderL5A("chk-ai-1", "sha256:ai1", true)); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	seedActiveCertification(t, store, "sha256:ai1", "cas-allinv", "chk-ai-1", "")
	seedActiveCertification(t, store, "sha256:ai2", "cas-allinv", "chk-ai-missing", "")

	n, err := svc.RetractUnprovenCertifications()
	if err != nil {
		t.Fatalf("RetractUnprovenCertifications: %v", err)
	}
	if n != 2 {
		t.Errorf("retracted = %d; want 2", n)
	}
	assertPromotion(t, store, "sha256:ai1", "cas-allinv", index.PromotionRetracted)
	assertPromotion(t, store, "sha256:ai2", "cas-allinv", index.PromotionRetracted)
}

// TestRetractUnprovenCertifications_RecertifiesOnNewEvidence verifies
// retraction is not permanent: a later admissible terminal L5-a check
// re-certifies the tool through the normal path.
func TestRetractUnprovenCertifications_RecertifiesOnNewEvidence(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)
	seedEntry(t, store, "cas-re", "sha256:re")

	if err := store.AppendToolCheckRecord(placeholderL5A("chk-re-ph", "sha256:re", true)); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	seedActiveCertification(t, store, "sha256:re", "cas-re", "chk-re-ph", "")
	if _, err := svc.RetractUnprovenCertifications(); err != nil {
		t.Fatalf("RetractUnprovenCertifications: %v", err)
	}
	assertPromotion(t, store, "sha256:re", "cas-re", index.PromotionRetracted)

	good := newCheckRecord("chk-re-good", "sha256:re", "bwa", "1.0", "succeeded")
	if err := store.AppendToolCheckRecord(good); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	if err := svc.EvaluateAfterCheck(good); err != nil {
		t.Fatalf("EvaluateAfterCheck: %v", err)
	}
	assertPromotion(t, store, "sha256:re", "cas-re", index.PromotionActive)
}
