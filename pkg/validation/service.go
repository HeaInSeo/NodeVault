// Package validation implements ValidationResultService — the gRPC receiver for
// ToolCheckRecord and ToolScanRecord submitted by NodeSentinel after L5 runs.
package validation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// Service implements ValidationResultServiceServer.
type Service struct {
	nfv1.UnimplementedValidationResultServiceServer
	store   *index.Store
	certSvc *certification.Service
}

// New creates a ValidationResultService backed by the given index store.
func New(store *index.Store, certSvc *certification.Service) *Service {
	return &Service{store: store, certSvc: certSvc}
}

// SubmitToolCheckRecord stores an L5-a functional validation result and
// triggers certification evaluation.
func (s *Service) SubmitToolCheckRecord(
	_ context.Context, req *nfv1.ToolCheckRecordRequest,
) (*nfv1.SubmitRecordResponse, error) {
	if req.CheckId == "" || req.ImageDigest == "" {
		return nil, status.Error(codes.InvalidArgument, "check_id and image_digest are required")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "index store not initialized")
	}

	checkedAt := time.Now().UTC()
	if req.CheckedAt > 0 {
		checkedAt = time.Unix(0, req.CheckedAt*int64(time.Millisecond)).UTC()
	}

	rec := index.ToolCheckRecord{
		CheckID:          req.CheckId,
		ToolSpecDigest:   req.ToolSpecDigest,
		ImageDigest:      req.ImageDigest,
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
		s.certSvc.EvaluateAfterCheck(rec)
		certStatus = "certified"
		if rec.ValidationStatus != "succeeded" {
			certStatus = "pending"
		}
	}

	return &nfv1.SubmitRecordResponse{
		RecordId:            req.CheckId,
		CertificationStatus: certStatus,
	}, nil
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

	scannedAt := time.Now().UTC()
	if req.ScannedAt > 0 {
		scannedAt = time.Unix(0, req.ScannedAt*int64(time.Millisecond)).UTC()
	}

	rec := index.ToolScanRecord{
		ScanID:         req.ScanId,
		ImageDigest:    req.ImageDigest,
		ToolName:       req.ToolName,
		Scanner:        req.Scanner,
		ScannerVersion: req.ScannerVersion,
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
		s.certSvc.EvaluateAfterScan(rec)
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
		filterStatus = index.PromotionStatus(req.PromotionStatus)
	}

	entries, err := s.store.ListToolFunctionCatalogEntries(filterStatus)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list catalog entries: %v", err)
	}

	resp := &nfv1.ListCertifiedToolsResponse{}
	for _, e := range entries {
		resp.Tools = append(resp.Tools, &nfv1.CertifiedToolEntry{
			CasHash:         e.CasHash,
			ToolName:        e.ToolName,
			Version:         e.Version,
			StableRef:       e.StableRef,
			ImageDigest:     e.ImageDigest,
			ImageRef:        e.ImageRef,
			DisplayLabel:    e.DisplayLabel,
			DisplayCategory: e.DisplayCategory,
			PromotionStatus: fmt.Sprintf("%s", e.PromotionStatus),
			CertifiedAt:     e.CertifiedAt.UnixMilli(),
			ValidationHash:  e.ValidationHash,
		})
	}
	return resp, nil
}
