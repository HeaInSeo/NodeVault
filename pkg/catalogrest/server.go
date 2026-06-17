// Package catalogrest provides the read-only Catalog HTTP REST service.
//
// Endpoints:
//
//	GET /v1/catalog/tools                     — list active tools (query: stable_ref, artifact_kind)
//	GET /v1/catalog/tools/{cas_hash}          — get single tool by CAS hash
//	GET /v1/catalog/data                      — list active data artifacts (query: stable_ref)
//	GET /v1/catalog/data/{cas_hash}           — get single data artifact by CAS hash
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
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// certService is the minimal interface used by the REST validation handlers to
// trigger certification evaluation after a ToolCheckRecord or ToolScanRecord
// is stored. Implemented by *certification.Service in production.
type certService interface {
	EvaluateAfterCheck(check index.ToolCheckRecord)
	EvaluateAfterScan(scan index.ToolScanRecord)
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
func NewMuxWithCert(store *index.Store, cat *catalog.Catalog, dataCat *catalog.DataCatalog, certSvc certService) *http.ServeMux {
	s := &Server{store: store, catalog: cat, dataCatalog: dataCat, certSvc: certSvc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/catalog/tools", s.handleListTools)
	mux.HandleFunc("GET /v1/catalog/tools/{cas_hash}", s.handleGetTool)
	mux.HandleFunc("GET /v1/catalog/data", s.handleListData)
	mux.HandleFunc("GET /v1/catalog/data/{cas_hash}", s.handleGetData)
	// Sprint 4: certified tool catalog (NodePalette primary source)
	mux.HandleFunc("GET /v1/catalog/certified-tools", s.handleListCertifiedTools)
	mux.HandleFunc("GET /v1/catalog/certified-tools/{cas_hash}", s.handleGetCertifiedTool)
	// Sprint 3: NodeSentinel → NodeVault validation record push (REST, avoids cross-repo gRPC)
	mux.HandleFunc("POST /v1/validation/check-records", s.handleSubmitCheckRecord)
	mux.HandleFunc("POST /v1/validation/scan-records", s.handleSubmitScanRecord)
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
// Query parameter: promotion_status (default "active")
func (s *Server) handleListCertifiedTools(w http.ResponseWriter, r *http.Request) {
	ps := r.URL.Query().Get("promotion_status")
	if ps == "" {
		ps = "active"
	}
	entries, err := s.store.ListToolFunctionCatalogEntries(index.PromotionStatus(ps))
	if err != nil {
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}
	items := make([]CertifiedToolItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, toCertifiedToolItem(e))
	}
	writeJSON(w, ListCertifiedToolsResponse{Tools: items})
}

// handleGetCertifiedTool serves GET /v1/catalog/certified-tools/{cas_hash}.
func (s *Server) handleGetCertifiedTool(w http.ResponseWriter, r *http.Request) {
	casHash := r.PathValue("cas_hash")
	entries, err := s.store.ListToolFunctionCatalogEntries("")
	if err != nil {
		http.Error(w, "index error", http.StatusInternalServerError)
		return
	}
	for _, e := range entries {
		if e.CasHash == casHash {
			writeJSON(w, toCertifiedToolItem(e))
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

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
	CheckID           string                   `json:"check_id"`
	ToolSpecDigest    string                   `json:"tool_spec_digest,omitempty"`
	ImageDigest       string                   `json:"image_digest"`
	ToolName          string                   `json:"tool_name,omitempty"`
	Version           string                   `json:"version,omitempty"`
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
	FailureReason     string                   `json:"failure_reason,omitempty"`
}

// SubmitScanRecordRequest is the JSON body for POST /v1/validation/scan-records.
type SubmitScanRecordRequest struct {
	ScanID         string `json:"scan_id"`
	ImageDigest    string `json:"image_digest"`
	ToolName       string `json:"tool_name,omitempty"`
	Scanner        string `json:"scanner,omitempty"`
	ScannerVersion string `json:"scanner_version,omitempty"`
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

	rec := index.ToolCheckRecord{
		CheckID:          req.CheckID,
		ToolSpecDigest:   req.ToolSpecDigest,
		ImageDigest:      req.ImageDigest,
		ToolName:         req.ToolName,
		Version:          req.Version,
		ValidationStatus: req.ValidationStatus,
		ValidationHash:   req.ValidationHash,
		Command:          req.Command,
		ExitCode:         req.ExitCode,
		FailureReason:    req.FailureReason,
		CheckedAt:        time.Now().UTC(),
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

	if err := s.store.AppendToolCheckRecord(rec); err != nil {
		slog.Error("store check record", "check_id", req.CheckID, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	slog.Info("ToolCheckRecord stored via REST", "check_id", req.CheckID, "status", req.ValidationStatus)

	certStatus := "pending"
	if s.certSvc != nil && req.ValidationStatus == "succeeded" {
		s.certSvc.EvaluateAfterCheck(rec)
		certStatus = "certified"
	}

	writeJSON(w, SubmitRecordResponse{RecordID: req.CheckID, CertificationStatus: certStatus})
}

// handleSubmitScanRecord serves POST /v1/validation/scan-records.
func (s *Server) handleSubmitScanRecord(w http.ResponseWriter, r *http.Request) {
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

	rec := index.ToolScanRecord{
		ScanID:         req.ScanID,
		ImageDigest:    req.ImageDigest,
		ToolName:       req.ToolName,
		Scanner:        req.Scanner,
		ScannerVersion: req.ScannerVersion,
		Source:         req.Source,
		CriticalCount:  req.CriticalCount,
		HighCount:      req.HighCount,
		MediumCount:    req.MediumCount,
		LowCount:       req.LowCount,
		PolicyMode:     req.PolicyMode,
		PolicyResult:   req.PolicyResult,
		ScannedAt:      time.Now().UTC(),
	}

	if err := s.store.AppendToolScanRecord(rec); err != nil {
		slog.Error("store scan record", "scan_id", req.ScanID, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	slog.Info("ToolScanRecord stored via REST", "scan_id", req.ScanID, "image_digest", req.ImageDigest)

	if s.certSvc != nil {
		s.certSvc.EvaluateAfterScan(rec)
	}

	writeJSON(w, SubmitRecordResponse{RecordID: req.ScanID, CertificationStatus: "pending"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
