package certification_test

import (
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// placeholderL5A mirrors what NodeSentinel's stub L5-a ("/bin/sh -c true")
// submits today: succeeded + passed contract result, but no validation hash
// and declared outputs not observed (NodeVault #117).
func placeholderL5A(checkID, imageDigest string, terminal bool) index.ToolCheckRecord {
	return index.ToolCheckRecord{
		CheckID:          checkID,
		ImageDigest:      imageDigest,
		ToolName:         "bwa",
		Version:          "1.0",
		Stage:            "L5A",
		Terminal:         terminal,
		ValidationStatus: "succeeded",
		Command:          "/bin/sh -c true",
		ContractCheck:    &index.ContractCheck{AllOutputsPresent: false, Result: "passed"},
		CheckedAt:        time.Now().UTC(),
	}
}

func assertNotCertified(t *testing.T, store *index.Store, imageDigest string) {
	t.Helper()
	if cert, err := store.GetCertifiedToolImageRecord(imageDigest); err == nil {
		t.Fatalf("image %s was certified with status %q; want no certification record", imageDigest, cert.PromotionStatus)
	}
	entries, err := store.ListToolFunctionCatalogEntries(index.PromotionActive)
	if err != nil {
		t.Fatalf("ListToolFunctionCatalogEntries: %v", err)
	}
	for i := range entries {
		if entries[i].ImageDigest == imageDigest {
			t.Fatalf("image %s has an ACTIVE catalog entry; want none", imageDigest)
		}
	}
}

func seedEntry(t *testing.T, store *index.Store, casHash, imageDigest string) {
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
		t.Fatalf("store.Append: %v", err)
	}
}

// TestEvaluateAfterCheck_InadmissibleRecordsNeverCertify is the #117
// regression: every succeeded record that is not a terminal, evidence-bearing
// L5-a result must leave the tool non-ACTIVE, even when a registered entry
// and a passing scan both exist.
func TestEvaluateAfterCheck_InadmissibleRecordsNeverCertify(t *testing.T) {
	withHash := func(r index.ToolCheckRecord) index.ToolCheckRecord {
		r.ValidationHash = "hash-" + r.CheckID
		return r
	}
	cases := []struct {
		name  string
		check index.ToolCheckRecord
	}{
		{"placeholder terminal L5-a", placeholderL5A("chk-ph", "sha256:ph", true)},
		{"placeholder with hash but outputs not observed", withHash(placeholderL5A("chk-noout", "sha256:noout", true))},
		{"evidence present but no contract check", func() index.ToolCheckRecord {
			r := newCheckRecord("chk-nocc", "sha256:nocc", "bwa", "1.0", "succeeded")
			r.ContractCheck = nil
			return r
		}()},
		{"contract result not passed", func() index.ToolCheckRecord {
			r := newCheckRecord("chk-ccfail", "sha256:ccfail", "bwa", "1.0", "succeeded")
			r.ContractCheck = &index.ContractCheck{AllOutputsPresent: true, Result: "failed"}
			return r
		}()},
		{"non-terminal evidence-bearing L5-a", func() index.ToolCheckRecord {
			r := newCheckRecord("chk-nonterm", "sha256:nonterm", "bwa", "1.0", "succeeded")
			r.Terminal = false
			return r
		}()},
		{"terminal L4 success", func() index.ToolCheckRecord {
			r := newCheckRecord("chk-l4", "sha256:l4", "bwa", "1.0", "succeeded")
			r.Stage = "L4"
			return r
		}()},
		{"no stage (gRPC wire shape)", func() index.ToolCheckRecord {
			r := newCheckRecord("chk-nostage", "sha256:nostage", "bwa", "1.0", "succeeded")
			r.Stage = ""
			return r
		}()},
		{"unknown stage literal", func() index.ToolCheckRecord {
			r := newCheckRecord("chk-l5a-lower", "sha256:l5alower", "bwa", "1.0", "succeeded")
			r.Stage = "l5a"
			return r
		}()},
		{"unknown status literal", newCheckRecord("chk-unk", "sha256:unk", "bwa", "1.0", "SUCCEEDED")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			svc := certification.New(store)
			seedEntry(t, store, "cas-"+tc.check.CheckID, tc.check.ImageDigest)
			if err := store.AppendToolScanRecord(index.ToolScanRecord{
				ScanID: "scan-" + tc.check.CheckID, ImageDigest: tc.check.ImageDigest,
				PolicyMode: "record_only", PolicyResult: "pass", ScannedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("AppendToolScanRecord: %v", err)
			}
			if err := store.AppendToolCheckRecord(tc.check); err != nil {
				t.Fatalf("AppendToolCheckRecord: %v", err)
			}
			if certification.CertifiesOnCheck(tc.check) {
				t.Error("CertifiesOnCheck = true; want false")
			}
			if err := svc.EvaluateAfterCheck(tc.check); err != nil {
				t.Fatalf("EvaluateAfterCheck: %v", err)
			}
			assertNotCertified(t, store, tc.check.ImageDigest)
		})
	}
}

// TestEvaluateAfterScan_PlaceholderCheckNeverCertifies verifies the scan path
// applies the same admission rule: a later L5-b scan must not promote a
// placeholder L5-a success.
func TestEvaluateAfterScan_PlaceholderCheckNeverCertifies(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)
	seedEntry(t, store, "cas-ph-scan", "sha256:phscan")

	if err := store.AppendToolCheckRecord(placeholderL5A("chk-ph-scan", "sha256:phscan", false)); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	scan := index.ToolScanRecord{
		ScanID: "scan-ph", ImageDigest: "sha256:phscan", Stage: "L5B", Terminal: true,
		PolicyMode: "record_only", PolicyResult: "pass", ScannedAt: time.Now().UTC(),
	}
	if err := store.AppendToolScanRecord(scan); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}
	if err := svc.EvaluateAfterScan(scan); err != nil {
		t.Fatalf("EvaluateAfterScan: %v", err)
	}
	assertNotCertified(t, store, "sha256:phscan")
}

// TestEvaluateAfterScan_NonTerminalEvidenceCheckCertifiesAfterL5B verifies a
// non-terminal evidence-bearing L5-a is deferred at check time and certified
// only once the L5-b scan arrives.
func TestEvaluateAfterScan_NonTerminalEvidenceCheckCertifiesAfterL5B(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)
	seedEntry(t, store, "cas-seq", "sha256:seq")

	check := newCheckRecord("chk-seq", "sha256:seq", "bwa", "1.0", "succeeded")
	check.Terminal = false
	if err := store.AppendToolCheckRecord(check); err != nil {
		t.Fatalf("AppendToolCheckRecord: %v", err)
	}
	if err := svc.EvaluateAfterCheck(check); err != nil {
		t.Fatalf("EvaluateAfterCheck: %v", err)
	}
	assertNotCertified(t, store, "sha256:seq")

	scan := index.ToolScanRecord{
		ScanID: "scan-seq", ImageDigest: "sha256:seq", Stage: "L5B", Terminal: true,
		PolicyMode: "record_only", PolicyResult: "pass", ScannedAt: time.Now().UTC(),
	}
	if err := store.AppendToolScanRecord(scan); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}
	if err := svc.EvaluateAfterScan(scan); err != nil {
		t.Fatalf("EvaluateAfterScan: %v", err)
	}
	cert, err := store.GetCertifiedToolImageRecord("sha256:seq")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.PromotionStatus != index.PromotionActive || cert.CheckID != "chk-seq" || cert.ScanID != "scan-seq" {
		t.Errorf("cert = {status %q, check %q, scan %q}; want {active, chk-seq, scan-seq}",
			cert.PromotionStatus, cert.CheckID, cert.ScanID)
	}
}

// TestEvaluateAfterScan_SkipsPlaceholderForLaterEvidence verifies the scan
// path picks the admissible check even when a placeholder was stored first.
func TestEvaluateAfterScan_SkipsPlaceholderForLaterEvidence(t *testing.T) {
	store := newStore(t)
	svc := certification.New(store)
	seedEntry(t, store, "cas-mix", "sha256:mix")

	if err := store.AppendToolCheckRecord(placeholderL5A("chk-mix-ph", "sha256:mix", false)); err != nil {
		t.Fatalf("AppendToolCheckRecord placeholder: %v", err)
	}
	good := newCheckRecord("chk-mix-good", "sha256:mix", "bwa", "1.0", "succeeded")
	good.Terminal = false
	if err := store.AppendToolCheckRecord(good); err != nil {
		t.Fatalf("AppendToolCheckRecord evidence: %v", err)
	}
	scan := index.ToolScanRecord{
		ScanID: "scan-mix", ImageDigest: "sha256:mix", PolicyResult: "pass", ScannedAt: time.Now().UTC(),
	}
	if err := store.AppendToolScanRecord(scan); err != nil {
		t.Fatalf("AppendToolScanRecord: %v", err)
	}
	if err := svc.EvaluateAfterScan(scan); err != nil {
		t.Fatalf("EvaluateAfterScan: %v", err)
	}
	cert, err := store.GetCertifiedToolImageRecord("sha256:mix")
	if err != nil {
		t.Fatalf("GetCertifiedToolImageRecord: %v", err)
	}
	if cert.CheckID != "chk-mix-good" {
		t.Errorf("cert.CheckID = %q; want chk-mix-good (the evidence-bearing check)", cert.CheckID)
	}
}

// TestCertifiesOnCheck_TerminalEvidenceL5A pins the one admissible shape.
func TestCertifiesOnCheck_TerminalEvidenceL5A(t *testing.T) {
	if !certification.CertifiesOnCheck(newCheckRecord("chk-ok", "sha256:ok", "bwa", "1.0", "succeeded")) {
		t.Error("CertifiesOnCheck = false for a terminal evidence-bearing L5-a success; want true")
	}
}
