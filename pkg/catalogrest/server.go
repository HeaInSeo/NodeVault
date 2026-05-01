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
	"net/http"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

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

// Server serves the read-only Catalog REST API.
type Server struct {
	store       *index.Store
	catalog     *catalog.Catalog
	dataCatalog *catalog.DataCatalog
}

// NewMux creates an http.ServeMux pre-wired with Catalog REST endpoints.
// The caller is responsible for binding it to an *http.Server.
func NewMux(store *index.Store, cat *catalog.Catalog, dataCat *catalog.DataCatalog) *http.ServeMux {
	s := &Server{store: store, catalog: cat, dataCatalog: dataCat}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/catalog/tools", s.handleListTools)
	mux.HandleFunc("GET /v1/catalog/tools/{cas_hash}", s.handleGetTool)
	mux.HandleFunc("GET /v1/catalog/data", s.handleListData)
	mux.HandleFunc("GET /v1/catalog/data/{cas_hash}", s.handleGetData)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
