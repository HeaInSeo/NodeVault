// Package catalogrest provides the read-only Catalog HTTP REST service.
//
// Endpoints:
//
//	GET /v1/catalog/tools                     — list active tools (query: stable_ref, artifact_kind)
//	GET /v1/catalog/tools/{cas_hash}          — get single tool by CAS hash
//	GET /v1/catalog/data                      — list active data artifacts (query: stable_ref)
//	GET /v1/catalog/data/{cas_hash}           — get single data artifact by CAS hash
//	GET /v1/palette/tools                     — alias for /v1/catalog/tools
//	GET /v1/palette/data                      — alias for /v1/catalog/data
//	GET /v1/gc/toolprofile-candidates         — list toolprofile referrers marked GC_CANDIDATE
//	                                             (query: subject_digest, optional)
//
// Catalog 노출 규칙: lifecycle_phase = Active 기준만.
// integrity_health는 이 서비스가 노출 결정에 사용하지 않는다.
package catalogrest

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// certService is the minimal interface used by the REST validation handlers to
// trigger certification evaluation after a ToolCheckRecord or ToolScanRecord
// is stored. Implemented by *certification.Service in production.
type certService interface {
	EvaluateAfterCheck(check index.ToolCheckRecord) error
	EvaluateAfterScan(scan index.ToolScanRecord) error
}

// ToolItem is the JSON wire format for a single registered tool.
type ToolItem struct {
	CasHash         string `json:"cas_hash"`
	ToolName        string `json:"tool_name"`
	Version         string `json:"version"`
	StableRef       string `json:"stable_ref"`
	ImageUri        string `json:"image_uri"`
	Digest          string `json:"digest"`
	LifecyclePhase  string `json:"lifecycle_phase"`
	IntegrityHealth string `json:"integrity_health"`
	RegisteredAt    int64  `json:"registered_at"`
	DisplayLabel    string `json:"display_label,omitempty"`
	DisplayCategory string `json:"display_category,omitempty"`
	Command         string `json:"command,omitempty"`
}

// ListToolsResponse is the JSON body for GET /v1/catalog/tools.
type ListToolsResponse struct {
	Tools []ToolItem `json:"tools"`
}

// DataItem is the JSON wire format for a single registered data artifact.
type DataItem struct {
	CasHash         string `json:"cas_hash"`
	DataName        string `json:"data_name"`
	Version         string `json:"version"`
	StableRef       string `json:"stable_ref"`
	Description     string `json:"description,omitempty"`
	Format          string `json:"format,omitempty"`
	SourceUri       string `json:"source_uri,omitempty"`
	Checksum        string `json:"checksum,omitempty"`
	StorageUri      string `json:"storage_uri,omitempty"`
	LifecyclePhase  string `json:"lifecycle_phase"`
	IntegrityHealth string `json:"integrity_health"`
	RegisteredAt    int64  `json:"registered_at"`
	DisplayLabel    string `json:"display_label,omitempty"`
	DisplayCategory string `json:"display_category,omitempty"`
}

// ListDataResponse is the JSON body for GET /v1/catalog/data.
type ListDataResponse struct {
	Data []DataItem `json:"data"`
}

// Server serves the Catalog REST API (read-only catalog + validation record intake).
type Server struct {
	store       *index.Store
	catalog     *catalog.Catalog
	dataCatalog *catalog.DataCatalog
	certSvc     certService // nil = no automatic certification trigger
}

// NewMux creates an http.ServeMux pre-wired with Catalog REST endpoints.
// The caller is responsible for binding it to an *http.Server.
func NewMux(store *index.Store, cat *catalog.Catalog, dataCat *catalog.DataCatalog) *http.ServeMux {
	return NewMuxWithCert(store, cat, dataCat, nil)
}

// NewMuxWithCert is like NewMux but wires in a certification.Service so that
// POST /v1/validation/check-records and POST /v1/validation/scan-records can
// trigger automatic certification evaluation after NodeSentinel submits records.
func NewMuxWithCert(
	store *index.Store, cat *catalog.Catalog, dataCat *catalog.DataCatalog, certSvc certService,
) *http.ServeMux {
	s := &Server{store: store, catalog: cat, dataCatalog: dataCat, certSvc: certSvc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/catalog/tools", s.handleListTools)
	mux.HandleFunc("GET /v1/catalog/tools/{cas_hash}", s.handleGetTool)
	mux.HandleFunc("GET /v1/catalog/data", s.handleListData)
	mux.HandleFunc("GET /v1/catalog/data/{cas_hash}", s.handleGetData)
	// NodePalette aliases for pipeline builders. The response schema is
	// identical to the catalog endpoints and includes cas_hash for execution pinning.
	mux.HandleFunc("GET /v1/palette/tools", s.handleListTools)
	mux.HandleFunc("GET /v1/palette/data", s.handleListData)
	// Sprint 4: certified tool catalog (NodePalette primary source)
	mux.HandleFunc("GET /v1/catalog/certified-tools", s.handleListCertifiedTools)
	mux.HandleFunc("GET /v1/catalog/certified-tools/{cas_hash}", s.handleGetCertifiedTool)
	// Sprint 3: NodeSentinel → NodeVault validation record push (REST, avoids cross-repo gRPC)
	mux.HandleFunc("POST /v1/validation/check-records", s.handleSubmitCheckRecord)
	mux.HandleFunc("POST /v1/validation/scan-records", s.handleSubmitScanRecord)
	// toolprofile referrer GC candidate visibility (index-local marking only;
	// see docs/OBSERVED_PROFILE_SPEC.md §5.2)
	mux.HandleFunc("GET /v1/gc/toolprofile-candidates", s.handleListToolProfileGCCandidates)
	return mux
}

// ── handlers ──────────────────────────────────────────────────────────────────

// handleListTools serves GET /v1/catalog/tools.
// Query parameters:
//   - stable_ref: filter by stable_ref (UI search key)
//   - artifact_kind: "tool" | "data" — empty returns all kinds
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	stableRef := r.URL.Query().Get("stable_ref")
	kind := r.URL.Query().Get("artifact_kind")

	var entries []index.Entry
	var err error
	if stableRef != "" {
		entries, err = s.store.ListByStableRef(stableRef)
	} else {
		entries, err = s.store.ListActive()
	}
	if err != nil {
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}

	items := make([]ToolItem, 0, len(entries))
	for i := range entries {
		if kind != "" && string(entries[i].ArtifactKind) != kind {
			continue
		}
		tool, loadErr := s.catalog.Load(entries[i].CasHash)
		if loadErr != nil {
			// CAS file missing — skip; reconcile loop will update integrity_health.
			continue
		}
		items = append(items, toToolItem(tool, entries[i].IntegrityHealth))
	}

	writeJSON(w, ListToolsResponse{Tools: items})
}

// handleGetTool serves GET /v1/catalog/tools/{cas_hash}.
func (s *Server) handleGetTool(w http.ResponseWriter, r *http.Request) {
	casHash := r.PathValue("cas_hash")
	if casHash == "" {
		http.Error(w, "cas_hash required", http.StatusBadRequest)
		return
	}

	entry, err := s.store.GetByCasHash(casHash)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}

	tool, err := s.catalog.Load(casHash)
	if err != nil {
		http.Error(w, "catalog load error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, toToolItem(tool, entry.IntegrityHealth))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func toToolItem(t *nfv1.RegisteredToolDefinition, health index.IntegrityHealth) ToolItem {
	item := ToolItem{
		CasHash:         t.CasHash,
		ToolName:        t.ToolName,
		Version:         t.Version,
		StableRef:       t.StableRef,
		ImageUri:        t.ImageUri,
		Digest:          t.Digest,
		LifecyclePhase:  t.LifecyclePhase,
		IntegrityHealth: string(health),
		RegisteredAt:    t.RegisteredAt,
		Command:         t.Command,
	}
	if t.Display != nil {
		item.DisplayLabel = t.Display.Label
		item.DisplayCategory = t.Display.Category
	}
	return item
}

// handleListData serves GET /v1/catalog/data.
// Query parameter: stable_ref (optional filter).
func (s *Server) handleListData(w http.ResponseWriter, r *http.Request) {
	stableRef := r.URL.Query().Get("stable_ref")

	var entries []index.Entry
	var err error
	if stableRef != "" {
		entries, err = s.store.ListByStableRef(stableRef)
	} else {
		entries, err = s.store.ListActive()
	}
	if err != nil {
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}

	items := make([]DataItem, 0)
	for i := range entries {
		if entries[i].ArtifactKind != index.KindData {
			continue
		}
		d, loadErr := s.dataCatalog.Load(entries[i].CasHash)
		if loadErr != nil {
			continue
		}
		items = append(items, toDataItem(d, entries[i].IntegrityHealth))
	}

	writeJSON(w, ListDataResponse{Data: items})
}

// handleGetData serves GET /v1/catalog/data/{cas_hash}.
func (s *Server) handleGetData(w http.ResponseWriter, r *http.Request) {
	casHash := r.PathValue("cas_hash")
	if casHash == "" {
		http.Error(w, "cas_hash required", http.StatusBadRequest)
		return
	}

	entry, err := s.store.GetByCasHash(casHash)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}
	if entry.ArtifactKind != index.KindData {
		http.Error(w, "not a data artifact", http.StatusNotFound)
		return
	}

	d, err := s.dataCatalog.Load(casHash)
	if err != nil {
		http.Error(w, "datacatalog load error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, toDataItem(d, entry.IntegrityHealth))
}

func toDataItem(d *nfv1.RegisteredDataDefinition, health index.IntegrityHealth) DataItem {
	item := DataItem{
		CasHash:         d.CasHash,
		DataName:        d.DataName,
		Version:         d.Version,
		StableRef:       d.StableRef,
		Description:     d.Description,
		Format:          d.Format,
		SourceUri:       d.SourceUri,
		Checksum:        d.Checksum,
		StorageUri:      d.StorageUri,
		LifecyclePhase:  d.LifecyclePhase,
		IntegrityHealth: string(health),
		RegisteredAt:    d.RegisteredAt,
	}
	if d.Display != nil {
		item.DisplayLabel = d.Display.Label
		item.DisplayCategory = d.Display.Category
	}
	return item
}

// ── certified tools (Sprint 4) ────────────────────────────────────────────────

// CertifiedToolItem is the JSON wire format for a certified tool catalog entry.
type CertifiedToolItem struct {
	CasHash            string   `json:"cas_hash"`
	ToolName           string   `json:"tool_name"`
	Version            string   `json:"version"`
	StableRef          string   `json:"stable_ref"`
	ImageDigest        string   `json:"image_digest"`
	ImageRef           string   `json:"image_ref,omitempty"`
	DisplayLabel       string   `json:"display_label,omitempty"`
	DisplayDescription string   `json:"display_description,omitempty"`
	DisplayCategory    string   `json:"display_category,omitempty"`
	DisplayTags        []string `json:"display_tags,omitempty"`
	PromotionStatus    string   `json:"promotion_status"`
	CertifiedAt        int64    `json:"certified_at"`
	ValidationHash     string   `json:"validation_hash,omitempty"`
}

// ListCertifiedToolsResponse is the JSON body for GET /v1/catalog/certified-tools.
type ListCertifiedToolsResponse struct {
	Tools []CertifiedToolItem `json:"tools"`
}

// handleListCertifiedTools serves GET /v1/catalog/certified-tools.
// Query parameter: promotion_status (default "active"; allowed: "active", "superseded", "retracted")
func (s *Server) handleListCertifiedTools(w http.ResponseWriter, r *http.Request) {
	ps := r.URL.Query().Get("promotion_status")
	if ps == "" {
		ps = "active"
	}
	switch ps {
	case string(index.PromotionActive), string(index.PromotionSuperseded), string(index.PromotionRetracted):
		// valid
	default:
		http.Error(w, "promotion_status must be one of: active, superseded, retracted", http.StatusBadRequest)
		return
	}
	entries, err := s.store.ListToolFunctionCatalogEntries(index.PromotionStatus(ps))
	if err != nil {
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}
	items := make([]CertifiedToolItem, 0, len(entries))
	for i := range entries {
		items = append(items, toCertifiedToolItem(entries[i]))
	}
	writeJSON(w, ListCertifiedToolsResponse{Tools: items})
}

// handleGetCertifiedTool serves GET /v1/catalog/certified-tools/{cas_hash}.
func (s *Server) handleGetCertifiedTool(w http.ResponseWriter, r *http.Request) {
	casHash := r.PathValue("cas_hash")
	if casHash == "" {
		http.Error(w, "cas_hash required", http.StatusBadRequest)
		return
	}
	entries, err := s.store.ListToolFunctionCatalogEntries("")
	if err != nil {
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}
	for i := range entries {
		if entries[i].CasHash == casHash {
			writeJSON(w, toCertifiedToolItem(entries[i]))
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

//nolint:gocritic // hugeParam: ToolFunctionCatalogEntry by value is intentional — callers own their copy.
func toCertifiedToolItem(e index.ToolFunctionCatalogEntry) CertifiedToolItem {
	return CertifiedToolItem{
		CasHash:            e.CasHash,
		ToolName:           e.ToolName,
		Version:            e.Version,
		StableRef:          e.StableRef,
		ImageDigest:        e.ImageDigest,
		ImageRef:           e.ImageRef,
		DisplayLabel:       e.DisplayLabel,
		DisplayDescription: e.DisplayDescription,
		DisplayCategory:    e.DisplayCategory,
		DisplayTags:        e.DisplayTags,
		PromotionStatus:    string(e.PromotionStatus),
		CertifiedAt:        e.CertifiedAt.UnixMilli(),
		ValidationHash:     e.ValidationHash,
	}
}

// ── validation record intake (Sprint 3, POST) ────────────────────────────────

// PortObservationRequest is the JSON wire type for a port I/O observation.
type PortObservationRequest struct {
	Port      string `json:"port"`
	FileCount int    `json:"file_count"`
	NonEmpty  bool   `json:"non_empty"`
}

// SubmitCheckRecordRequest is the JSON body for POST /v1/validation/check-records.
type SubmitCheckRecordRequest struct {
	CheckID        string `json:"check_id"`
	ToolSpecDigest string `json:"tool_spec_digest,omitempty"`
	ImageDigest    string `json:"image_digest"`
	Platform       string `json:"platform,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	Version        string `json:"version,omitempty"`

	// See index.ToolCheckRecord's doc comments — same correlation/stage/
	// terminal/failure-classification contract carried over the wire.
	ValidationRequestID string `json:"validation_request_id,omitempty"`
	SentinelJobID       string `json:"sentinel_job_id,omitempty"`
	Stage               string `json:"stage,omitempty"`
	Terminal            bool   `json:"terminal,omitempty"`

	ValidationStatus  string                   `json:"validation_status"`
	ValidationHash    string                   `json:"validation_hash,omitempty"`
	Command           string                   `json:"command,omitempty"`
	ExitCode          int                      `json:"exit_code,omitempty"`
	ObservedInputs    []PortObservationRequest `json:"observed_inputs,omitempty"`
	ObservedOutputs   []PortObservationRequest `json:"observed_outputs,omitempty"`
	PeakCPUMilli      int64                    `json:"peak_cpu_millicores,omitempty"`
	PeakMemoryMiB     int64                    `json:"peak_memory_mib,omitempty"`
	DurationSeconds   int64                    `json:"duration_seconds,omitempty"`
	Timeout           bool                     `json:"timeout,omitempty"`
	AllOutputsPresent bool                     `json:"all_outputs_present,omitempty"`
	ContractResult    string                   `json:"contract_result,omitempty"`

	FailureKind   string `json:"failure_kind,omitempty"`
	FailureCode   string `json:"failure_code,omitempty"`
	Retryable     bool   `json:"retryable,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// SubmitScanRecordRequest is the JSON body for POST /v1/validation/scan-records.
type SubmitScanRecordRequest struct {
	ScanID      string `json:"scan_id"`
	ImageDigest string `json:"image_digest"`
	ToolName    string `json:"tool_name,omitempty"`
	Platform    string `json:"platform,omitempty"`

	ValidationRequestID string `json:"validation_request_id,omitempty"`
	SentinelJobID       string `json:"sentinel_job_id,omitempty"`
	Stage               string `json:"stage,omitempty"`
	Terminal            bool   `json:"terminal,omitempty"`

	Scanner        string `json:"scanner,omitempty"`
	ScannerVersion string `json:"scanner_version,omitempty"`
	DbDigest       string `json:"db_digest,omitempty"`
	Source         string `json:"source,omitempty"`
	CriticalCount  int    `json:"critical_count"`
	HighCount      int    `json:"high_count"`
	MediumCount    int    `json:"medium_count"`
	LowCount       int    `json:"low_count"`
	PolicyMode     string `json:"policy_mode,omitempty"`
	PolicyResult   string `json:"policy_result,omitempty"`
}

// SubmitRecordResponse is the JSON response for both POST validation endpoints.
type SubmitRecordResponse struct {
	RecordID            string `json:"record_id"`
	CertificationStatus string `json:"certification_status"`
}

// handleSubmitCheckRecord serves POST /v1/validation/check-records.
func (s *Server) handleSubmitCheckRecord(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req SubmitCheckRecordRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CheckID == "" || req.ImageDigest == "" {
		http.Error(w, "check_id and image_digest required", http.StatusBadRequest)
		return
	}
	if !s.checkValidationCorrelation(w, req.ValidationRequestID, req.ImageDigest) {
		return
	}

	rec := index.ToolCheckRecord{
		CheckID:             req.CheckID,
		ToolSpecDigest:      req.ToolSpecDigest,
		ImageDigest:         req.ImageDigest,
		Platform:            req.Platform,
		ToolName:            req.ToolName,
		Version:             req.Version,
		ValidationRequestID: req.ValidationRequestID,
		SentinelJobID:       req.SentinelJobID,
		Stage:               req.Stage,
		Terminal:            req.Terminal,
		ValidationStatus:    req.ValidationStatus,
		ValidationHash:      req.ValidationHash,
		Command:             req.Command,
		ExitCode:            req.ExitCode,
		FailureKind:         req.FailureKind,
		FailureCode:         req.FailureCode,
		Retryable:           req.Retryable,
		FailureReason:       req.FailureReason,
		CheckedAt:           time.Now().UTC(),
	}
	if len(req.ObservedInputs) > 0 || len(req.ObservedOutputs) > 0 {
		iop := &index.ObservedIoProfile{}
		for _, p := range req.ObservedInputs {
			iop.Inputs = append(iop.Inputs, index.PortObservation{Port: p.Port, FileCount: p.FileCount, NonEmpty: p.NonEmpty})
		}
		for _, p := range req.ObservedOutputs {
			iop.Outputs = append(iop.Outputs, index.PortObservation{Port: p.Port, FileCount: p.FileCount, NonEmpty: p.NonEmpty})
		}
		rec.ObservedIoProfile = iop
	}
	if req.DurationSeconds > 0 || req.PeakCPUMilli > 0 || req.Timeout {
		rec.ObservedResourceProfile = &index.ObservedResourceProfile{
			PeakCPUMillicores: req.PeakCPUMilli,
			PeakMemoryMiB:     req.PeakMemoryMiB,
			DurationSeconds:   req.DurationSeconds,
			Timeout:           req.Timeout,
		}
	}
	if req.ContractResult != "" {
		rec.ContractCheck = &index.ContractCheck{
			AllOutputsPresent: req.AllOutputsPresent,
			Result:            req.ContractResult,
		}
	}

	succeeded := req.ValidationStatus == "succeeded"
	if err := s.store.AppendToolCheckRecordCorrelated(
		rec, req.ValidationRequestID, req.SentinelJobID, req.Terminal, succeeded, req.FailureReason,
	); err != nil {
		if errors.Is(err, index.ErrDuplicateRecord) {
			slog.Info("ToolCheckRecord already recorded (idempotent redelivery)", "check_id", req.CheckID)
			writeJSON(w, SubmitRecordResponse{RecordID: req.CheckID, CertificationStatus: "already_recorded"})
			return
		}
		slog.Error("store check record", "check_id", req.CheckID, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	slog.Info("ToolCheckRecord stored via REST",
		"check_id", req.CheckID, "status", req.ValidationStatus, "stage", req.Stage, "terminal", req.Terminal)

	certStatus := "pending"
	if s.certSvc != nil && succeeded {
		if err := s.certSvc.EvaluateAfterCheck(rec); err != nil {
			slog.Error("certification failed after check", "check_id", req.CheckID, "err", err)
			certStatus = "failed"
		} else {
			certStatus = "certified"
		}
	}

	writeJSON(w, SubmitRecordResponse{RecordID: req.CheckID, CertificationStatus: certStatus})
}

// checkValidationCorrelation resolves validationRequestID against this
// store's ValidationRequestRecords and reports the outcome via the
// nodevault_validation_correlation_* metrics.
//
// A missing ID or one with no matching record (orphan) is fail-open: logs a
// warning, bumps a metric, and returns true so the caller still stores the
// record — see index.ValidationRequestRecord's accepted orphan-record gap
// (PR2-A). Losing the validation *result* entirely because correlation
// bookkeeping has a gap would be worse than storing it uncorrelated.
//
// Only a *found* record whose ImageDigest doesn't match imageDigest is a
// real integrity problem — that means this result is for a different image
// than what NodeVault actually asked NodeSentinel to validate. That writes
// a 409 response and returns false; the caller must not store anything.
func (s *Server) checkValidationCorrelation(w http.ResponseWriter, validationRequestID, imageDigest string) bool {
	if validationRequestID == "" {
		metrics.ValidationCorrelationMissingIDTotal.Add(1)
		return true
	}
	rec, err := s.store.GetValidationRequestRecord(validationRequestID)
	if err != nil {
		slog.Warn("validation correlation: no matching ValidationRequestRecord (orphan)",
			"validation_request_id", validationRequestID)
		metrics.ValidationCorrelationOrphanTotal.Add(1)
		return true
	}
	if rec.ImageDigest != "" && rec.ImageDigest != imageDigest {
		slog.Warn("validation correlation: image_digest mismatch — rejecting",
			"validation_request_id", validationRequestID, "expected", rec.ImageDigest, "got", imageDigest)
		metrics.ValidationCorrelationDigestMismatchTotal.Add(1)
		http.Error(w, "image_digest does not match the validation_request_id's recorded request", http.StatusConflict)
		return false
	}
	metrics.ValidationCorrelationMatchedTotal.Add(1)
	return true
}

// handleSubmitScanRecord serves POST /v1/validation/scan-records.
func (s *Server) handleSubmitScanRecord(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req SubmitScanRecordRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.ScanID == "" || req.ImageDigest == "" {
		http.Error(w, "scan_id and image_digest required", http.StatusBadRequest)
		return
	}
	if !s.checkValidationCorrelation(w, req.ValidationRequestID, req.ImageDigest) {
		return
	}

	rec := index.ToolScanRecord{
		ScanID:              req.ScanID,
		ImageDigest:         req.ImageDigest,
		ToolName:            req.ToolName,
		Platform:            req.Platform,
		ValidationRequestID: req.ValidationRequestID,
		SentinelJobID:       req.SentinelJobID,
		Stage:               req.Stage,
		Terminal:            req.Terminal,
		Scanner:             req.Scanner,
		ScannerVersion:      req.ScannerVersion,
		DbDigest:            req.DbDigest,
		Source:              req.Source,
		CriticalCount:       req.CriticalCount,
		HighCount:           req.HighCount,
		MediumCount:         req.MediumCount,
		LowCount:            req.LowCount,
		PolicyMode:          req.PolicyMode,
		PolicyResult:        req.PolicyResult,
		ScannedAt:           time.Now().UTC(),
	}

	// A blocked policy result is the only scan outcome that fails the
	// overall validation request when this record is Terminal — a warning
	// or an unavailable scanner (Source == "not-available") does not.
	succeeded := req.PolicyResult != "blocked"
	if err := s.store.AppendToolScanRecordCorrelated(
		rec, req.ValidationRequestID, req.SentinelJobID, req.Terminal, succeeded,
	); err != nil {
		if errors.Is(err, index.ErrDuplicateRecord) {
			slog.Info("ToolScanRecord already recorded (idempotent redelivery)", "scan_id", req.ScanID)
			writeJSON(w, SubmitRecordResponse{RecordID: req.ScanID, CertificationStatus: "already_recorded"})
			return
		}
		slog.Error("store scan record", "scan_id", req.ScanID, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	slog.Info("ToolScanRecord stored via REST",
		"scan_id", req.ScanID, "image_digest", req.ImageDigest, "terminal", req.Terminal)

	if s.certSvc != nil {
		if err := s.certSvc.EvaluateAfterScan(rec); err != nil {
			slog.Error("certification failed after scan", "scan_id", req.ScanID, "err", err)
		}
	}

	writeJSON(w, SubmitRecordResponse{RecordID: req.ScanID, CertificationStatus: "pending"})
}

// ── toolprofile referrer GC candidates ───────────────────────────────────────

// ToolProfileReferrerItem is the JSON wire format for one toolprofile referrer
// and its NodeVault-local GC marking state.
type ToolProfileReferrerItem struct {
	Digest           string `json:"digest"`
	ValidationRunID  string `json:"validation_run_id,omitempty"`
	ValidationStatus string `json:"validation_status,omitempty"`
	ValidatedAt      int64  `json:"validated_at"`
	Rank             int    `json:"rank"`
	LifecycleStatus  string `json:"lifecycle_status"`
	GCReason         string `json:"gc_reason,omitempty"`
	MarkedAt         int64  `json:"marked_at,omitempty"`
}

// ListToolProfileGCCandidatesResponse is the JSON body for
// GET /v1/gc/toolprofile-candidates.
type ListToolProfileGCCandidatesResponse struct {
	Candidates []ToolProfileReferrerItem `json:"candidates"`
}

// handleListToolProfileGCCandidates serves GET /v1/gc/toolprofile-candidates.
//
// This is a read-only view of NodeVault's local GC marking — it never
// triggers a registry call. Physical deletion of GC_CANDIDATE referrers is
// delegated to Harbor retention/GC policy, operators, or an external cleanup
// runner. See docs/OBSERVED_PROFILE_SPEC.md §5.2.
//
// Query parameter: subject_digest — filter to one image digest (optional;
// omitting it lists candidates across all entries).
func (s *Server) handleListToolProfileGCCandidates(w http.ResponseWriter, r *http.Request) {
	subjectDigest := r.URL.Query().Get("subject_digest")

	var entries []index.Entry
	if subjectDigest != "" {
		entry, err := s.store.GetByImageDigest(subjectDigest)
		if err != nil {
			if errors.Is(err, index.ErrNotFound) {
				writeJSON(w, ListToolProfileGCCandidatesResponse{Candidates: []ToolProfileReferrerItem{}})
				return
			}
			http.Error(w, "index error", http.StatusInternalServerError)
			return
		}
		entries = []index.Entry{entry}
	} else {
		all, err := s.store.All()
		if err != nil {
			http.Error(w, "index error", http.StatusInternalServerError)
			return
		}
		entries = all
	}

	items := make([]ToolProfileReferrerItem, 0)
	for i := range entries {
		cands, err := s.store.ListToolProfileGCCandidates(entries[i].CasHash)
		if err != nil {
			continue
		}
		for j := range cands {
			items = append(items, toToolProfileReferrerItem(&cands[j]))
		}
	}

	writeJSON(w, ListToolProfileGCCandidatesResponse{Candidates: items})
}

func toToolProfileReferrerItem(r *index.ToolProfileReferrer) ToolProfileReferrerItem {
	item := ToolProfileReferrerItem{
		Digest:           r.Digest,
		ValidationRunID:  r.ValidationRunID,
		ValidationStatus: r.ValidationStatus,
		ValidatedAt:      r.ValidatedAt.UnixMilli(),
		Rank:             r.Rank,
		LifecycleStatus:  string(r.LifecycleStatus),
		GCReason:         r.GCReason,
	}
	if !r.MarkedAt.IsZero() {
		item.MarkedAt = r.MarkedAt.UnixMilli()
	}
	return item
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON encode", "err", err)
	}
}
