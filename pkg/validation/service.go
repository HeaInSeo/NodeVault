// Package validation implements ValidationResultService — the gRPC receiver for
// ToolCheckRecord and ToolScanRecord submitted by NodeSentinel after L5 runs.
package validation

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/oras"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// certificationService is the minimal interface for triggering certification
// after a validation record is stored. Implemented by *certification.Service.
type certificationService interface {
	EvaluateAfterCheck(check index.ToolCheckRecord) error
	EvaluateAfterScan(scan index.ToolScanRecord) error
}

// checkedAtMaxMs is the maximum valid checked_at / scanned_at value in milliseconds
// (9999-12-31T23:59:59.999Z expressed as Unix milliseconds).
const checkedAtMaxMs = int64(253402300799999)

// Service implements ValidationResultServiceServer.
type Service struct {
	nfv1.UnimplementedValidationResultServiceServer
	store   *index.Store
	certSvc certificationService
}

// New creates a ValidationResultService backed by the given index store.
func New(store *index.Store, certSvc certificationService) *Service {
	return &Service{store: store, certSvc: certSvc}
}

// SubmitToolCheckRecord stores an L5-a functional validation result and
// triggers certification evaluation.
func (s *Service) SubmitToolCheckRecord(
	ctx context.Context, req *nfv1.ToolCheckRecordRequest,
) (*nfv1.SubmitRecordResponse, error) {
	if req.CheckId == "" || req.ImageDigest == "" {
		return nil, status.Error(codes.InvalidArgument, "check_id and image_digest are required")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "index store not initialized")
	}

	// checked_at is Unix milliseconds; 0 means "use server time".
	// Values outside [0, 253402300799999] are rejected to prevent silent timestamp corruption.
	if req.CheckedAt < 0 || req.CheckedAt > checkedAtMaxMs {
		return nil, status.Errorf(codes.InvalidArgument,
			"checked_at out of range: must be 0 or a Unix millisecond timestamp in [0, %d]", checkedAtMaxMs)
	}

	checkedAt := time.Now().UTC()
	if req.CheckedAt > 0 {
		checkedAt = time.Unix(0, req.CheckedAt*int64(time.Millisecond)).UTC()
	}

	rec := index.ToolCheckRecord{
		CheckID:          req.CheckId,
		ToolSpecDigest:   req.ToolSpecDigest,
		ImageDigest:      req.ImageDigest,
		Platform:         req.Platform,
		ToolName:         req.ToolName,
		Version:          req.Version,
		ValidationStatus: req.ValidationStatus,
		ValidationHash:   req.ValidationHash,
		Command:          req.Command,
		ExitCode:         int(req.ExitCode),
		FailureReason:    req.FailureReason,
		CheckedAt:        checkedAt,
	}
	if req.ObservedIoProfile != nil {
		iop := &index.ObservedIoProfile{}
		for _, p := range req.ObservedIoProfile.Inputs {
			iop.Inputs = append(iop.Inputs, index.PortObservation{
				Port: p.Port, FileCount: int(p.FileCount), NonEmpty: p.NonEmpty,
			})
		}
		for _, p := range req.ObservedIoProfile.Outputs {
			iop.Outputs = append(iop.Outputs, index.PortObservation{
				Port: p.Port, FileCount: int(p.FileCount), NonEmpty: p.NonEmpty,
			})
		}
		rec.ObservedIoProfile = iop
	}
	if req.ObservedResourceProfile != nil {
		rp := req.ObservedResourceProfile
		rec.ObservedResourceProfile = &index.ObservedResourceProfile{
			PeakCPUMillicores: rp.PeakCpuMillicores,
			PeakMemoryMiB:     rp.PeakMemoryMib,
			DurationSeconds:   rp.DurationSeconds,
			DiskReadMiB:       rp.DiskReadMib,
			DiskWriteMiB:      rp.DiskWriteMib,
			Timeout:           rp.Timeout,
			TimeoutSeconds:    rp.TimeoutSeconds,
		}
	}
	if req.ContractCheck != nil {
		rec.ContractCheck = &index.ContractCheck{
			AllOutputsPresent: req.ContractCheck.AllOutputsPresent,
			Result:            req.ContractCheck.Result,
		}
	}

	if err := s.store.AppendToolCheckRecord(rec); err != nil {
		slog.Error("failed to store ToolCheckRecord", "check_id", req.CheckId, "err", err)
		return nil, status.Errorf(codes.Internal, "store check record: %v", err)
	}
	slog.Info("ToolCheckRecord stored", "check_id", req.CheckId, "status", req.ValidationStatus)

	certStatus := "pending"
	if s.certSvc != nil {
		if err := s.certSvc.EvaluateAfterCheck(rec); err != nil {
			slog.Error("certification failed after check", "check_id", req.CheckId, "err", err)
			certStatus = "failed"
		} else if rec.ValidationStatus == "succeeded" {
			certStatus = "certified"
		}
	}

	s.pushProfileReferrer(ctx, rec)

	return &nfv1.SubmitRecordResponse{
		RecordId:            req.CheckId,
		CertificationStatus: certStatus,
	}, nil
}

// pushProfileReferrer attaches an observed validation profile as a Harbor OCI
// referrer for a successful check record, then records it in the index so
// NodeVault can compute GC_CANDIDATE marking locally (see
// docs/OBSERVED_PROFILE_SPEC.md §5.2). Non-fatal: if the push fails or the
// index.Entry isn't found yet, ObservedProfileDigest simply stays unset until
// a future validation run retries.
//
//nolint:gocritic // hugeParam: ToolCheckRecord by value matches AppendToolCheckRecord's existing convention.
func (s *Service) pushProfileReferrer(ctx context.Context, rec index.ToolCheckRecord) {
	if rec.ValidationStatus != "succeeded" || rec.ValidationHash == "" {
		return
	}
	entry, err := s.store.GetByImageDigest(rec.ImageDigest)
	if err != nil {
		return
	}
	imageRepo := imageRepoFromRef(entry.ImageRef)
	referrerDigest, err := oras.PushToolProfileReferrer(ctx, imageRepo, rec.ImageDigest, entry.CasHash, &rec)
	if err != nil {
		slog.Warn("toolprofile referrer push failed", "check_id", rec.CheckID, "err", err)
		return
	}
	ref := index.ToolProfileReferrer{
		Digest:           referrerDigest,
		ValidationRunID:  rec.CheckID,
		ValidationStatus: rec.ValidationStatus,
		ValidatedAt:      rec.CheckedAt,
		ObservedAt:       time.Now().UTC(),
	}
	if _, err := s.store.RecordToolProfileReferrer(entry.CasHash, &ref); err != nil {
		slog.Warn("index toolprofile referrer record failed", "err", err)
		return
	}
	slog.Info("toolprofile referrer attached", "check_id", rec.CheckID, "referrer", referrerDigest)
}

// imageRepoFromRef strips the tag from a full "host[:port]/project/repo:tag"
// reference (index.Entry.ImageRef), returning the bare repository reference.
func imageRepoFromRef(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// SubmitToolScanRecord stores an L5-b security scan result and triggers
// certification re-evaluation.
func (s *Service) SubmitToolScanRecord(
	_ context.Context, req *nfv1.ToolScanRecordRequest,
) (*nfv1.SubmitRecordResponse, error) {
	if req.ScanId == "" || req.ImageDigest == "" {
		return nil, status.Error(codes.InvalidArgument, "scan_id and image_digest are required")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "index store not initialized")
	}

	// scanned_at is Unix milliseconds; 0 means "use server time".
	// Values outside [0, 253402300799999] are rejected to prevent silent timestamp corruption.
	if req.ScannedAt < 0 || req.ScannedAt > checkedAtMaxMs {
		return nil, status.Errorf(codes.InvalidArgument,
			"scanned_at out of range: must be 0 or a Unix millisecond timestamp in [0, %d]", checkedAtMaxMs)
	}

	scannedAt := time.Now().UTC()
	if req.ScannedAt > 0 {
		scannedAt = time.Unix(0, req.ScannedAt*int64(time.Millisecond)).UTC()
	}

	rec := index.ToolScanRecord{
		ScanID:         req.ScanId,
		ImageDigest:    req.ImageDigest,
		ToolName:       req.ToolName,
		Platform:       req.Platform,
		Scanner:        req.Scanner,
		ScannerVersion: req.ScannerVersion,
		DbDigest:       req.DbDigest,
		Source:         req.Source,
		CriticalCount:  int(req.CriticalCount),
		HighCount:      int(req.HighCount),
		MediumCount:    int(req.MediumCount),
		LowCount:       int(req.LowCount),
		PolicyMode:     req.PolicyMode,
		PolicyResult:   req.PolicyResult,
		ScannedAt:      scannedAt,
	}

	if err := s.store.AppendToolScanRecord(rec); err != nil {
		slog.Error("failed to store ToolScanRecord", "scan_id", req.ScanId, "err", err)
		return nil, status.Errorf(codes.Internal, "store scan record: %v", err)
	}
	slog.Info("ToolScanRecord stored", "scan_id", req.ScanId, "image_digest", req.ImageDigest)

	if s.certSvc != nil {
		if err := s.certSvc.EvaluateAfterScan(rec); err != nil {
			slog.Error("certification failed after scan", "scan_id", req.ScanId, "err", err)
		}
	}

	return &nfv1.SubmitRecordResponse{
		RecordId:            req.ScanId,
		CertificationStatus: "pending",
	}, nil
}

// ListCertifiedTools returns active certified tool catalog entries.
func (s *Service) ListCertifiedTools(
	_ context.Context, req *nfv1.ListCertifiedToolsRequest,
) (*nfv1.ListCertifiedToolsResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "index store not initialized")
	}

	filterStatus := index.PromotionActive
	if req.PromotionStatus != "" {
		switch req.PromotionStatus {
		case string(index.PromotionActive), string(index.PromotionSuperseded), string(index.PromotionRetracted):
			filterStatus = index.PromotionStatus(req.PromotionStatus)
		default:
			return nil, status.Errorf(codes.InvalidArgument,
				"promotion_status %q is not valid; allowed values: active, superseded, retracted",
				req.PromotionStatus)
		}
	}

	entries, err := s.store.ListActiveToolFunctionCatalogEntries(filterStatus)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list catalog entries: %v", err)
	}

	resp := &nfv1.ListCertifiedToolsResponse{}
	for i := range entries {
		e := &entries[i]
		resp.Tools = append(resp.Tools, &nfv1.CertifiedToolEntry{
			CasHash:         e.CasHash,
			ToolName:        e.ToolName,
			Version:         e.Version,
			StableRef:       e.StableRef,
			ImageDigest:     e.ImageDigest,
			ImageRef:        e.ImageRef,
			DisplayLabel:    e.DisplayLabel,
			DisplayCategory: e.DisplayCategory,
			PromotionStatus: string(e.PromotionStatus),
			CertifiedAt:     e.CertifiedAt.UnixMilli(),
			ValidationHash:  e.ValidationHash,
		})
	}
	return resp, nil
}
