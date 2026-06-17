// Package certification evaluates validation records and produces
// CertifiedToolImageRecord + ToolFunctionCatalogEntry decisions.
//
// Authority: certification is determined by NodeVault alone, after receiving
// ToolCheckRecord and ToolScanRecord from NodeSentinel. NodeSentinel produces
// records; NodeVault decides certification.
package certification

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// Service evaluates validation records and writes certification decisions to the index.
type Service struct {
	store *index.Store
}

// New creates a Certification Service backed by the given index store.
func New(store *index.Store) *Service {
	return &Service{store: store}
}

// EvaluateAfterCheck is called after a ToolCheckRecord is stored.
// If the check succeeded, it immediately attempts certification using any
// existing scan record (or proceeds without one if scan is optional).
//
//nolint:gocritic // hugeParam: ToolCheckRecord by value matches the certService interface contract.
func (s *Service) EvaluateAfterCheck(check index.ToolCheckRecord) error {
	if check.ValidationStatus != "succeeded" {
		slog.Info("certification skipped: check not succeeded",
			"check_id", check.CheckID, "status", check.ValidationStatus)
		return nil
	}

	scans, err := s.store.ListToolScanRecordsByImageDigest(check.ImageDigest)
	if err != nil {
		slog.Error("certification aborted: failed to list scan records",
			"check_id", check.CheckID, "err", err)
		return fmt.Errorf("list scan records: %w", err)
	}
	var latestScan *index.ToolScanRecord
	for i := range scans {
		if latestScan == nil || scans[i].ScannedAt.After(latestScan.ScannedAt) {
			latestScan = &scans[i]
		}
	}

	return s.certify(check, latestScan)
}

// EvaluateAfterScan is called after a ToolScanRecord is stored.
// Looks for an existing successful check and re-evaluates certification.
//
//nolint:gocritic // hugeParam: ToolScanRecord by value matches the certService interface contract.
func (s *Service) EvaluateAfterScan(scan index.ToolScanRecord) error {
	checks, err := s.store.ListToolCheckRecordsByImageDigest(scan.ImageDigest)
	if err != nil {
		slog.Error("certification aborted: failed to list check records",
			"scan_id", scan.ScanID, "err", err)
		return fmt.Errorf("list check records: %w", err)
	}
	for i := range checks {
		if checks[i].ValidationStatus == "succeeded" {
			return s.certify(checks[i], &scan)
		}
	}
	slog.Info("certification deferred: no successful check record yet",
		"scan_id", scan.ScanID, "image_digest", scan.ImageDigest)
	return nil
}

// certify creates or updates CertifiedToolImageRecord and ToolFunctionCatalogEntry.
//
//nolint:gocritic // hugeParam: ToolCheckRecord by value is intentional — callers own their copy.
func (s *Service) certify(check index.ToolCheckRecord, scan *index.ToolScanRecord) error {
	if check.ToolName == "" || check.Version == "" {
		slog.Error("certification aborted: ToolName or Version is empty",
			"check_id", check.CheckID, "tool_name", check.ToolName, "version", check.Version)
		return fmt.Errorf("certify: ToolName and Version must not be empty (check_id=%q)", check.CheckID)
	}

	policyBlocked := false
	var scanID string
	if scan != nil {
		scanID = scan.ScanID
		if scan.PolicyResult == "blocked" {
			policyBlocked = true
			slog.Warn("certification blocked by security policy",
				"scan_id", scan.ScanID, "policy_mode", scan.PolicyMode)
		}
	}

	promotionStatus := index.PromotionActive
	if policyBlocked {
		promotionStatus = index.PromotionRetracted
	}

	cert := index.CertifiedToolImageRecord{
		ImageDigest:     check.ImageDigest,
		ToolSpecDigest:  check.ToolSpecDigest,
		ToolName:        check.ToolName,
		Version:         check.Version,
		PromotionStatus: promotionStatus,
		CertifiedAt:     time.Now().UTC(),
		CheckID:         check.CheckID,
		ScanID:          scanID,
	}

	// Lookup CasHash from index Entry if available (best-effort).
	stableRef := fmt.Sprintf("%s@%s", check.ToolName, check.Version)
	if entries, err := s.store.ListByStableRef(stableRef); err == nil && len(entries) > 0 {
		cert.CasHash = entries[0].CasHash
	}

	if err := s.store.UpsertCertifiedToolImageRecord(cert); err != nil {
		slog.Error("failed to store CertifiedToolImageRecord", "err", err)
		return fmt.Errorf("upsert certified tool image record: %w", err)
	}
	slog.Info("tool certified", "image_digest", cert.ImageDigest, "status", cert.PromotionStatus)

	if cert.CasHash == "" {
		slog.Warn("no CasHash found for certified tool — catalog entry skipped",
			"tool_name", check.ToolName, "version", check.Version)
		return nil
	}

	// Look up display metadata from existing catalog entry.
	imageRef := ""
	displayLabel := check.ToolName
	displayCategory := ""
	var displayTags []string
	if imgRec, err := s.store.GetToolImageRecordByDigest(check.ImageDigest); err == nil {
		imageRef = imgRec.ImageRef
	}

	catalogEntry := index.ToolFunctionCatalogEntry{
		CasHash:         cert.CasHash,
		ToolName:        check.ToolName,
		Version:         check.Version,
		StableRef:       fmt.Sprintf("%s@%s", check.ToolName, check.Version),
		ImageDigest:     check.ImageDigest,
		ImageRef:        imageRef,
		DisplayLabel:    displayLabel,
		DisplayCategory: displayCategory,
		DisplayTags:     displayTags,
		PromotionStatus: promotionStatus,
		CertifiedAt:     cert.CertifiedAt,
		ValidationHash:  check.ValidationHash,
	}
	if err := s.store.UpsertToolFunctionCatalogEntry(catalogEntry); err != nil {
		slog.Error("failed to store ToolFunctionCatalogEntry", "err", err)
		return fmt.Errorf("upsert tool function catalog entry: %w", err)
	}
	slog.Info("catalog entry upserted", "cas_hash", cert.CasHash, "status", promotionStatus)
	return nil
}
