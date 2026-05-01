// Package catalog manages RegisteredToolDefinition CAS storage and ToolRegistryService.
// Files are stored as {sha256-hash}.tooldefinition for content-addressable lookup.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const defaultCatalogDir = "assets/catalog"

// Catalog stores RegisteredToolDefinition objects as content-addressed files.
type Catalog struct {
	dir string
}

// NewCatalog creates a Catalog. CATALOG_DIR env overrides the default directory.
// NewCatalog creates a Catalog. CATALOG_DIR env overrides the default directory.
func NewCatalog() *Catalog {
	return NewCatalogAt("")
}

// NewCatalogAt creates a Catalog rooted at dir.
// If dir is empty, CATALOG_DIR env or the built-in default is used.
func NewCatalogAt(dir string) *Catalog {
	if dir == "" {
		dir = os.Getenv("CATALOG_DIR")
	}
	if dir == "" {
		dir = defaultCatalogDir
	}
	dir = filepath.Clean(dir)
	//nolint:gosec // dir is an operator-controlled storage root, intentionally configurable.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "catalog: mkdir %s: %v\n", dir, err)
	}
	return &Catalog{dir: dir}
}

// Save marshals tool to JSON, computes SHA256 of that JSON, and writes
// {hash}.tooldefinition. Returns the hex-encoded hash used as the CAS key.
// Note: if tool.CasHash is already set it contributes to the hash; use
// SaveWithCasHash to get stable CAS semantics (hash derived from content
// without CasHash, stored once with CasHash included).
func (c *Catalog) Save(tool *nfv1.RegisteredToolDefinition) (string, error) {
	data, err := json.Marshal(tool)
	if err != nil {
		return "", fmt.Errorf("marshal tool: %w", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	path := filepath.Join(c.dir, hash+".tooldefinition")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return hash, nil
}

// SaveWithCasHash computes the CAS hash from the tool's content (excluding
// the cas_hash field itself), sets tool.CasHash, and writes exactly one
// {hash}.tooldefinition file. Returns the stable CAS hash.
// This is the correct method to use from RegisterTool.
func (c *Catalog) SaveWithCasHash(tool *nfv1.RegisteredToolDefinition) (string, error) {
	// 1. Marshal without CasHash to get the stable content hash.
	prev := tool.CasHash
	tool.CasHash = ""
	contentData, err := json.Marshal(tool)
	tool.CasHash = prev // restore so caller is not surprised
	if err != nil {
		return "", fmt.Errorf("marshal for hash: %w", err)
	}
	sum := sha256.Sum256(contentData)
	hash := hex.EncodeToString(sum[:])

	// 2. Set CasHash and write once under that hash.
	tool.CasHash = hash
	fullData, err := json.Marshal(tool)
	if err != nil {
		return "", fmt.Errorf("marshal with cas_hash: %w", err)
	}
	path := filepath.Join(c.dir, hash+".tooldefinition")
	if err := os.WriteFile(path, fullData, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return hash, nil
}

// Load reads a single .tooldefinition file by CAS hash.
// Returns an error if the file does not exist.
func (c *Catalog) Load(casHash string) (*nfv1.RegisteredToolDefinition, error) {
	path := filepath.Join(c.dir, casHash+".tooldefinition")
	//nolint:gosec // casHash is a hex string derived internally; not from user input directly.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalog load %s: %w", casHash, err)
	}
	var t nfv1.RegisteredToolDefinition
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("catalog parse %s: %w", casHash, err)
	}
	return &t, nil
}

// List reads all *.tooldefinition files and returns the parsed tools.
//
//nolint:dupl // Catalog.List and DataCatalog.List are intentionally parallel — same structure, different proto types.
func (c *Catalog) List() ([]*nfv1.RegisteredToolDefinition, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("read catalog dir: %w", err)
	}
	tools := make([]*nfv1.RegisteredToolDefinition, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tooldefinition") {
			continue
		}
		path := filepath.Join(c.dir, filepath.Base(e.Name()))
		//nolint:gosec // file names come from ReadDir on the catalog root and are reduced to a base name.
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var t nfv1.RegisteredToolDefinition
		if jerr := json.Unmarshal(data, &t); jerr != nil {
			continue
		}
		tools = append(tools, &t)
	}
	return tools, nil
}

// ListActive returns only tools with lifecycle_phase == "Active".
// Catalog 노출 규칙: lifecycle_phase = Active 기준만. integrity_health는 영향 없음.
func (c *Catalog) ListActive() ([]*nfv1.RegisteredToolDefinition, error) {
	all, err := c.List()
	if err != nil {
		return nil, err
	}
	out := make([]*nfv1.RegisteredToolDefinition, 0, len(all))
	for _, t := range all {
		if t.LifecyclePhase == "Active" {
			out = append(out, t)
		}
	}
	return out, nil
}

// ListByStableRef returns all tools matching the given stable_ref.
// UI 검색·카탈로그 탐색 전용. 파이프라인 pin에는 casHash를 사용한다.
func (c *Catalog) ListByStableRef(stableRef string) ([]*nfv1.RegisteredToolDefinition, error) {
	all, err := c.List()
	if err != nil {
		return nil, err
	}
	out := make([]*nfv1.RegisteredToolDefinition, 0)
	for _, t := range all {
		if t.StableRef == stableRef {
			out = append(out, t)
		}
	}
	return out, nil
}

// ToolRegistryService implements ToolRegistryServiceServer.
// It dual-writes: full spec to CAS (catalog) and lightweight entry to index.Store.
type ToolRegistryService struct {
	nfv1.UnimplementedToolRegistryServiceServer
	catalog *Catalog
	store   *index.Store
}

// NewToolRegistryService creates a ToolRegistryService backed by the given Catalog and index.Store.
func NewToolRegistryService(cat *Catalog, store *index.Store) *ToolRegistryService {
	return &ToolRegistryService{catalog: cat, store: store}
}

// RegisterTool creates a RegisteredToolDefinition, saves it to the CAS catalog,
// and appends a lightweight entry to the index.Store.
func (s *ToolRegistryService) RegisterTool(
	_ context.Context, req *nfv1.RegisterToolRequest,
) (*nfv1.RegisterToolResponse, error) {
	stableRef := req.StableRef
	if stableRef == "" && req.ToolName != "" {
		// NodeVault가 tool_name@version 형태로 조립한다.
		if req.Version != "" {
			stableRef = req.ToolName + "@" + req.Version
		} else {
			stableRef = req.ToolName
		}
	}

	tool := &nfv1.RegisteredToolDefinition{
		ToolDefinitionId: req.ToolDefinitionId,
		ToolName:         req.ToolName,
		ImageUri:         req.ImageUri,
		Digest:           req.Digest,
		EnvironmentSpec:  req.EnvironmentSpec,
		RegisteredAt:     time.Now().Unix(),
		Version:          req.Version,
		StableRef:        stableRef,
		Inputs:           req.Inputs,
		Outputs:          req.Outputs,
		Display:          req.Display,
		Command:          req.Command,
		LifecyclePhase:   string(index.PhaseActive),
		IntegrityHealth:  string(index.HealthPartial), // Partial until spec referrer is pushed (pkg/oras)
		Validation: &nfv1.ValidationStatus{
			Phase:           "Passed",
			LastValidatedAt: time.Now().Unix(),
		},
	}

	hash, err := s.catalog.SaveWithCasHash(tool)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "catalog save: %v", err)
	}

	// Dual-write: append lightweight entry to the index.
	// index.ErrNotFound means the entry didn't exist yet (expected); duplicate is silently OK.
	entry := index.Entry{
		CasHash:         hash,
		ArtifactKind:    index.KindTool,
		StableRef:       stableRef,
		ToolName:        req.ToolName,
		Version:         req.Version,
		ImageRef:        req.ImageUri,
		ImageDigest:     req.Digest,
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthPartial, // Partial until spec referrer is pushed (pkg/oras)
	}
	if appendErr := s.store.Append(entry); appendErr != nil {
		// Duplicate is not a fatal error — the CAS file is the source of truth until
		// the full index transition (TODO-09b). Log and continue.
		fmt.Fprintf(os.Stderr, "catalog: index append %s: %v\n", hash, appendErr)
	}

	return &nfv1.RegisterToolResponse{CasHash: hash, Tool: tool}, nil
}

// ListTools returns registered tools, filtered by stable_ref and/or artifact_kind.
// Catalog 노출 규칙: index.ListActive() 기준 (lifecycle_phase = Active only).
func (s *ToolRegistryService) ListTools(
	_ context.Context, req *nfv1.ListToolsRequest,
) (*nfv1.ListToolsResponse, error) {
	var indexEntries []index.Entry
	var err error

	if req.GetStableRef() != "" {
		indexEntries, err = s.store.ListByStableRef(req.GetStableRef())
	} else {
		indexEntries, err = s.store.ListActive()
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "index list: %v", err)
	}

	// artifact_kind filter: empty = all kinds.
	kind := req.GetArtifactKind()

	tools := make([]*nfv1.RegisteredToolDefinition, 0, len(indexEntries))
	for i := range indexEntries {
		if kind != "" && string(indexEntries[i].ArtifactKind) != kind {
			continue
		}
		tool, loadErr := s.catalog.Load(indexEntries[i].CasHash)
		if loadErr != nil {
			// CAS file missing — log and skip; integrity_health reconcile will catch this.
			fmt.Fprintf(os.Stderr, "catalog: load %s: %v\n", indexEntries[i].CasHash, loadErr)
			continue
		}
		tools = append(tools, tool)
	}
	return &nfv1.ListToolsResponse{Tools: tools}, nil
}

// GetTool retrieves a single RegisteredToolDefinition by its CAS hash.
// Uses the index for existence check, then loads the full spec from the CAS catalog.
//
//nolint:dupl // GetTool and DataRegistryService.GetData are intentionally parallel — different proto types.
func (s *ToolRegistryService) GetTool(
	_ context.Context, req *nfv1.GetToolRequest,
) (*nfv1.RegisteredToolDefinition, error) {
	if _, err := s.store.GetByCasHash(req.CasHash); err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "tool %s not found", req.CasHash)
		}
		return nil, status.Errorf(codes.Internal, "index lookup: %v", err)
	}
	tool, err := s.catalog.Load(req.CasHash)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "catalog load: %v", err)
	}
	return tool, nil
}

// RetractTool transitions lifecycle_phase → Retracted.
// NodeVault only — not callable by reconcile loop.
// Retracted artifacts are excluded from Catalog listing (lifecycle_phase != Active).
//
//nolint:dupl // RetractTool and DeleteTool are intentionally parallel — same shape, different phases.
func (s *ToolRegistryService) RetractTool(
	_ context.Context, req *nfv1.RetractToolRequest,
) (*nfv1.RetractToolResponse, error) {
	if err := s.store.SetLifecyclePhase(req.CasHash, index.PhaseRetracted); err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "tool %s not found", req.CasHash)
		}
		return nil, status.Errorf(codes.Internal, "retract: %v", err)
	}
	return &nfv1.RetractToolResponse{
		CasHash:        req.CasHash,
		LifecyclePhase: string(index.PhaseRetracted),
	}, nil
}

// DeleteTool transitions lifecycle_phase → Deleted.
// Retracted → Deleted is the recommended sequence.
// NodeVault only.
//
//nolint:dupl // DeleteTool and RetractTool are intentionally parallel — same shape, different phases.
func (s *ToolRegistryService) DeleteTool(
	_ context.Context, req *nfv1.DeleteToolRequest,
) (*nfv1.DeleteToolResponse, error) {
	if err := s.store.SetLifecyclePhase(req.CasHash, index.PhaseDeleted); err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "tool %s not found", req.CasHash)
		}
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &nfv1.DeleteToolResponse{
		CasHash:        req.CasHash,
		LifecyclePhase: string(index.PhaseDeleted),
	}, nil
}
