// Package catalog — DataCatalog and DataRegistryService.
// Symmetric with Catalog / ToolRegistryService but for reference data artifacts.
// Files are stored as {sha256-hash}.datadefinition for content-addressable lookup.
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

const defaultDataCatalogDir = "assets/datacatalog"

// DataCatalog stores RegisteredDataDefinition objects as content-addressed files.
type DataCatalog struct {
	dir string
}

// NewDataCatalog creates a DataCatalog. DATA_CATALOG_DIR env overrides the default.
func NewDataCatalog() *DataCatalog {
	return NewDataCatalogAt("")
}

// NewDataCatalogAt creates a DataCatalog rooted at the given directory.
// If dir is empty, DATA_CATALOG_DIR env or the built-in default is used.
// Intended for test helpers that need an isolated temp directory.
func NewDataCatalogAt(dir string) *DataCatalog {
	if dir == "" {
		dir = os.Getenv("DATA_CATALOG_DIR")
	}
	if dir == "" {
		dir = defaultDataCatalogDir
	}
	dir = filepath.Clean(dir)
	//nolint:gosec // dir is an operator-controlled storage root, intentionally configurable.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "datacatalog: mkdir %s: %v\n", dir, err)
	}
	return &DataCatalog{dir: dir}
}

// SaveWithCasHash computes the CAS hash from the data spec content (excluding cas_hash),
// sets d.CasHash, and writes exactly one {hash}.datadefinition file.
func (c *DataCatalog) SaveWithCasHash(d *nfv1.RegisteredDataDefinition) (string, error) {
	prev := d.CasHash
	d.CasHash = ""
	contentData, err := json.Marshal(d)
	d.CasHash = prev
	if err != nil {
		return "", fmt.Errorf("marshal for hash: %w", err)
	}
	sum := sha256.Sum256(contentData)
	hash := hex.EncodeToString(sum[:])

	d.CasHash = hash
	fullData, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal with cas_hash: %w", err)
	}
	path := filepath.Join(c.dir, hash+".datadefinition")
	if err := os.WriteFile(path, fullData, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return hash, nil
}

// Load reads a single .datadefinition file by CAS hash.
func (c *DataCatalog) Load(casHash string) (*nfv1.RegisteredDataDefinition, error) {
	path := filepath.Join(c.dir, casHash+".datadefinition")
	//nolint:gosec // casHash is a hex string derived internally.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("datacatalog load %s: %w", casHash, err)
	}
	var d nfv1.RegisteredDataDefinition
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("datacatalog parse %s: %w", casHash, err)
	}
	return &d, nil
}

// List reads all *.datadefinition files and returns the parsed definitions.
//
//nolint:dupl // DataCatalog.List and Catalog.List are intentionally parallel — same structure, different proto types.
func (c *DataCatalog) List() ([]*nfv1.RegisteredDataDefinition, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("read datacatalog dir: %w", err)
	}
	out := make([]*nfv1.RegisteredDataDefinition, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".datadefinition") {
			continue
		}
		path := filepath.Join(c.dir, filepath.Base(e.Name()))
		//nolint:gosec // file names come from ReadDir on the datacatalog root.
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var d nfv1.RegisteredDataDefinition
		if jerr := json.Unmarshal(data, &d); jerr != nil {
			continue
		}
		out = append(out, &d)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DataRegistryService
// ─────────────────────────────────────────────────────────────────────────────

// DataRegistryService implements DataRegistryServiceServer.
// Dual-writes: full spec to DataCatalog (CAS) and lightweight entry to index.Store.
type DataRegistryService struct {
	nfv1.UnimplementedDataRegistryServiceServer
	cat   *DataCatalog
	store *index.Store
}

// NewDataRegistryService creates a DataRegistryService.
func NewDataRegistryService(cat *DataCatalog, store *index.Store) *DataRegistryService {
	return &DataRegistryService{cat: cat, store: store}
}

// RegisterData creates a RegisteredDataDefinition, saves it to the CAS datacatalog,
// and appends a lightweight entry to the index.Store with ArtifactKind = "data".
func (s *DataRegistryService) RegisterData(
	_ context.Context, req *nfv1.DataRegisterRequest,
) (*nfv1.DataRegisterResponse, error) {
	stableRef := req.StableRef
	if stableRef == "" && req.DataName != "" {
		if req.Version != "" {
			stableRef = req.DataName + "@" + req.Version
		} else {
			stableRef = req.DataName
		}
	}

	d := &nfv1.RegisteredDataDefinition{
		DataName:        req.DataName,
		Version:         req.Version,
		Description:     req.Description,
		Format:          req.Format,
		SourceUri:       req.SourceUri,
		Checksum:        req.Checksum,
		StorageUri:      req.StorageUri,
		StableRef:       stableRef,
		Display:         req.Display,
		RegisteredAt:    time.Now().Unix(),
		LifecyclePhase:  "Active",
		IntegrityHealth: "Healthy",
	}

	hash, err := s.cat.SaveWithCasHash(d)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "datacatalog save: %v", err)
	}

	entry := index.Entry{
		CasHash:         hash,
		ArtifactKind:    index.KindData,
		StableRef:       stableRef,
		ToolName:        req.DataName,
		Version:         req.Version,
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}
	if appendErr := s.store.Append(entry); appendErr != nil {
		fmt.Fprintf(os.Stderr, "datacatalog: index append %s: %v\n", hash, appendErr)
	}

	return &nfv1.DataRegisterResponse{CasHash: hash, Data: d}, nil
}

// GetData retrieves a single RegisteredDataDefinition by its CAS hash.
//
//nolint:dupl // GetData and ToolRegistryService.GetTool are intentionally parallel — different proto types.
func (s *DataRegistryService) GetData(
	_ context.Context, req *nfv1.GetDataRequest,
) (*nfv1.RegisteredDataDefinition, error) {
	if _, err := s.store.GetByCasHash(req.CasHash); err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "data %s not found", req.CasHash)
		}
		return nil, status.Errorf(codes.Internal, "index lookup: %v", err)
	}
	d, err := s.cat.Load(req.CasHash)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "datacatalog load: %v", err)
	}
	return d, nil
}

// ListData returns registered data artifacts, optionally filtered by stable_ref.
func (s *DataRegistryService) ListData(
	_ context.Context, req *nfv1.ListDataRequest,
) (*nfv1.ListDataResponse, error) {
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

	out := make([]*nfv1.RegisteredDataDefinition, 0, len(indexEntries))
	for i := range indexEntries {
		// ListActive returns all kinds; filter to data only.
		if indexEntries[i].ArtifactKind != index.KindData {
			continue
		}
		d, loadErr := s.cat.Load(indexEntries[i].CasHash)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "datacatalog: load %s: %v\n", indexEntries[i].CasHash, loadErr)
			continue
		}
		out = append(out, d)
	}
	return &nfv1.ListDataResponse{Data: out}, nil
}
