package validation_test

import (
	"context"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/validation"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// TestSubmitToolCheckRecord_SucceededDoesNotCertify is the gRPC half of #117:
// with the production certification.Service wired in, a succeeded record —
// even one carrying a validation hash and a passed contract check — cannot
// reach PromotionActive, because the gRPC wire has no stage/terminal and so
// never identifies a terminal L5-a result. REST and gRPC share one rule.
func TestSubmitToolCheckRecord_SucceededDoesNotCertify(t *testing.T) {
	store := newStore(t)
	if err := store.Append(index.Entry{
		CasHash:         "cas-grpc",
		ArtifactKind:    index.KindTool,
		StableRef:       "bwa@1.0",
		ToolName:        "bwa",
		Version:         "1.0",
		ImageDigest:     "sha256:grpc",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}); err != nil {
		t.Fatalf("store.Append: %v", err)
	}
	svc := validation.New(store, certification.New(store))

	resp, err := svc.SubmitToolCheckRecord(context.Background(), &nfv1.ToolCheckRecordRequest{
		CheckId:          "chk-grpc",
		ImageDigest:      "sha256:grpc",
		ToolName:         "bwa",
		Version:          "1.0",
		ValidationStatus: "succeeded",
		ValidationHash:   "vh-grpc",
		Command:          "/bin/sh -c true",
		ContractCheck:    &nfv1.ContractCheck{AllOutputsPresent: true, Result: "passed"},
	})
	if err != nil {
		t.Fatalf("SubmitToolCheckRecord: %v", err)
	}
	if resp.CertificationStatus != "pending" {
		t.Errorf("CertificationStatus = %q; want pending", resp.CertificationStatus)
	}
	if cert, err := store.GetCertifiedToolImageRecord("sha256:grpc"); err == nil {
		t.Fatalf("gRPC succeeded record certified the tool with status %q; want no certification", cert.PromotionStatus)
	}
}
