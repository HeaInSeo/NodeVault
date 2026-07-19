package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// schemaVersion 3 adds ToolCheckRecords, ToolScanRecords,
	// CertifiedToolImageRecords, and ToolFunctionCatalogEntries.
	// Older files omit these fields; load() treats absent fields as empty slices.
	schemaVersion   = 3
	defaultIndexDir = "assets/index"
	indexFileName   = "vault-index.json"
)

// Store is the single control layer for the NodeVault artifact index.
// All index reads and writes MUST go through this type.
// Direct access to the underlying file from other packages is forbidden.
//
// State transition rules (enforced by callers, not the store itself):
//   - SetLifecyclePhase: called only by NodeVault explicit operations.
//   - SetIntegrityHealth: called only by the reconcile loop.
type Store struct {
	mu   sync.RWMutex
	path string     // path to vault-index.json
	idx  *indexFile // in-memory cache; nil before first load
}

// ErrNotFound is returned when a requested entry does not exist.
var ErrNotFound = errors.New("index: entry not found")

// New creates a Store backed by the JSON file at dir/vault-index.json.
// The directory is created if it does not exist.
// INDEX_DIR env overrides the default directory.
func New() (*Store, error) {
	dir := os.Getenv("INDEX_DIR")
	if dir == "" {
		dir = defaultIndexDir
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("index: mkdir %s: %w", dir, err)
	}
	s := &Store{path: filepath.Join(dir, indexFileName)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewAt creates a Store at a specific path — useful for testing.
func NewAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("index: mkdir %s: %w", dir, err)
	}
	s := &Store{path: filepath.Join(dir, indexFileName)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Append adds a new entry to the index.
// Returns an error if an entry with the same CasHash already exists.
//
//nolint:gocritic // hugeParam: Entry by value is intentional — callers own their copy.
func (s *Store) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e.CasHash == "" {
		return errors.New("index: CasHash must not be empty")
	}
	for i := range s.idx.Entries {
		if s.idx.Entries[i].CasHash == e.CasHash {
			return fmt.Errorf("index: entry %q already exists", e.CasHash)
		}
	}
	now := time.Now().UTC()
	if e.RegisteredAt.IsZero() {
		e.RegisteredAt = now
	}
	if e.LifecycleUpdatedAt.IsZero() {
		e.LifecycleUpdatedAt = now
	}
	if e.HealthCheckedAt.IsZero() {
		e.HealthCheckedAt = now
	}
	s.idx.Entries = append(s.idx.Entries, e)
	return s.save()
}

// GetByCasHash returns the entry with the given CAS hash.
// Returns ErrNotFound if no such entry exists.
func (s *Store) GetByCasHash(casHash string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.Entries {
		if s.idx.Entries[i].CasHash == casHash {
			return s.idx.Entries[i], nil
		}
	}
	return Entry{}, fmt.Errorf("%w: cas_hash=%q", ErrNotFound, casHash)
}

// GetByImageDigest returns the first entry whose ImageDigest matches the given digest.
// Returns ErrNotFound if no such entry exists.
func (s *Store) GetByImageDigest(digest string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.Entries {
		if s.idx.Entries[i].ImageDigest == digest {
			return s.idx.Entries[i], nil
		}
	}
	return Entry{}, fmt.Errorf("%w: image_digest=%q", ErrNotFound, digest)
}

// ListByStableRef returns all entries with the given stableRef.
// Returns an empty slice (not an error) if none match.
func (s *Store) ListByStableRef(stableRef string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Entry
	for i := range s.idx.Entries {
		if s.idx.Entries[i].StableRef == stableRef {
			out = append(out, s.idx.Entries[i])
		}
	}
	return out, nil
}

// ListActive returns all entries with lifecycle_phase == Active.
// Catalog exposure rule: Active only. IntegrityHealth is not checked here.
func (s *Store) ListActive() ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Entry
	for i := range s.idx.Entries {
		if s.idx.Entries[i].LifecyclePhase == PhaseActive {
			out = append(out, s.idx.Entries[i])
		}
	}
	return out, nil
}

// SetLifecyclePhase updates the lifecycle_phase of the entry identified by casHash.
//
// IMPORTANT: This method MUST be called only by NodeVault explicit operations
// (register, retract, delete). The reconcile loop must never call this method.
func (s *Store) SetLifecyclePhase(casHash string, phase LifecyclePhase) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, err := s.findIndex(casHash)
	if err != nil {
		return err
	}
	s.idx.Entries[idx].LifecyclePhase = phase
	s.idx.Entries[idx].LifecycleUpdatedAt = time.Now().UTC()
	return s.save()
}

// SetSpecReferrerDigest records the OCI referrer digest after a successful spec push.
// Called by pkg/oras after PushToolSpecReferrer or PushDataSpecReferrer succeeds.
func (s *Store) SetSpecReferrerDigest(casHash, referrerDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, err := s.findIndex(casHash)
	if err != nil {
		return err
	}
	s.idx.Entries[idx].SpecReferrerDigest = referrerDigest
	return s.save()
}

// RecordToolProfileReferrer appends a newly pushed toolprofile referrer to the
// entry's history and re-ranks the full set to compute GC_CANDIDATE marking.
// Called by pkg/validation after PushToolProfileReferrer succeeds.
//
// Ranking never depends on registry Referrers() listing order: it sorts by
// ref.ValidatedAt (validation completion time), then ref.ObservedAt, then
// Digest, all descending/deterministic. This call only updates the NodeVault
// index — it never pushes, re-pushes, or deletes any OCI referrer manifest.
func (s *Store) RecordToolProfileReferrer(casHash string, ref *ToolProfileReferrer) ([]ToolProfileReferrer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, err := s.findIndex(casHash)
	if err != nil {
		return nil, err
	}
	e := &s.idx.Entries[idx]
	e.ObservedProfileReferrers = append(e.ObservedProfileReferrers, *ref)
	rankToolProfileReferrers(e.ObservedProfileReferrers)
	for i := range e.ObservedProfileReferrers {
		if e.ObservedProfileReferrers[i].Rank == 1 {
			e.ObservedProfileDigest = e.ObservedProfileReferrers[i].Digest
			break
		}
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	return append([]ToolProfileReferrer(nil), e.ObservedProfileReferrers...), nil
}

// ListToolProfileGCCandidates returns the GC_CANDIDATE-marked referrers for
// the entry identified by casHash, most-recently-marked first.
func (s *Store) ListToolProfileGCCandidates(casHash string) ([]ToolProfileReferrer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	idx, err := s.findIndex(casHash)
	if err != nil {
		return nil, err
	}
	return gcCandidates(s.idx.Entries[idx].ObservedProfileReferrers), nil
}

// rankToolProfileReferrers sorts refs most-recent-first by ValidatedAt, then
// ObservedAt, then Digest (a deterministic tie-breaker), assigns 1-based Rank,
// and marks every referrer beyond DefaultToolProfileReferrerRetain as
// GC_CANDIDATE. It never touches the registry.
func rankToolProfileReferrers(refs []ToolProfileReferrer) {
	sort.SliceStable(refs, func(i, j int) bool {
		a, b := refs[i], refs[j]
		if !a.ValidatedAt.Equal(b.ValidatedAt) {
			return a.ValidatedAt.After(b.ValidatedAt)
		}
		if !a.ObservedAt.Equal(b.ObservedAt) {
			return a.ObservedAt.After(b.ObservedAt)
		}
		return a.Digest < b.Digest
	})
	now := time.Now().UTC()
	for i := range refs {
		refs[i].Rank = i + 1
		if i < DefaultToolProfileReferrerRetain {
			refs[i].LifecycleStatus = ReferrerActive
			refs[i].GCReason = ""
			refs[i].MarkedAt = time.Time{}
			continue
		}
		if refs[i].LifecycleStatus != ReferrerGCCandidate {
			refs[i].MarkedAt = now
		}
		refs[i].LifecycleStatus = ReferrerGCCandidate
		refs[i].GCReason = "retained_limit_exceeded"
	}
}

func gcCandidates(refs []ToolProfileReferrer) []ToolProfileReferrer {
	var out []ToolProfileReferrer
	for i := range refs {
		if refs[i].LifecycleStatus == ReferrerGCCandidate {
			out = append(out, refs[i])
		}
	}
	return out
}

// SetIntegrityHealth updates the integrity_health of the entry identified by casHash.
//
// IMPORTANT: This method MUST be called only by the reconcile loop.
// Lifecycle operations (register, retract, delete) must never call this method.
func (s *Store) SetIntegrityHealth(casHash string, health IntegrityHealth) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, err := s.findIndex(casHash)
	if err != nil {
		return err
	}
	s.idx.Entries[idx].IntegrityHealth = health
	s.idx.Entries[idx].HealthCheckedAt = time.Now().UTC()
	return s.save()
}

// All returns a snapshot of all entries. Safe for read-only iteration.
func (s *Store) All() ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, len(s.idx.Entries))
	copy(out, s.idx.Entries)
	return out, nil
}

// ── ResolvedToolSpec ──────────────────────────────────────────────────────────

// AppendResolvedToolSpec adds a new resolved tool spec to the index.
// Returns an error if an entry with the same ToolSpecDigest already exists.
//
//nolint:dupl,gocritic // dupl: same guard+append pattern, distinct types. gocritic: by value is intentional.
func (s *Store) AppendResolvedToolSpec(r ResolvedToolSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ToolSpecDigest == "" {
		return errors.New("index: ToolSpecDigest must not be empty")
	}
	for i := range s.idx.ResolvedToolSpecs {
		if s.idx.ResolvedToolSpecs[i].ToolSpecDigest == r.ToolSpecDigest {
			return fmt.Errorf("index: resolved tool spec %q already exists", r.ToolSpecDigest)
		}
	}
	if r.ResolvedAt.IsZero() {
		r.ResolvedAt = time.Now().UTC()
	}
	s.idx.ResolvedToolSpecs = append(s.idx.ResolvedToolSpecs, r)
	return s.save()
}

// UpsertResolvedToolSpec inserts a new resolved tool spec or returns the existing
// record with the same ToolSpecDigest. Existing records are not mutated.
//
//nolint:gocritic // hugeParam: ResolvedToolSpec by value is intentional — callers own their copy.
func (s *Store) UpsertResolvedToolSpec(r ResolvedToolSpec) (ResolvedToolSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ToolSpecDigest == "" {
		return ResolvedToolSpec{}, errors.New("index: ToolSpecDigest must not be empty")
	}
	for i := range s.idx.ResolvedToolSpecs {
		if s.idx.ResolvedToolSpecs[i].ToolSpecDigest == r.ToolSpecDigest {
			return s.idx.ResolvedToolSpecs[i], nil
		}
	}
	if r.ResolvedAt.IsZero() {
		r.ResolvedAt = time.Now().UTC()
	}
	s.idx.ResolvedToolSpecs = append(s.idx.ResolvedToolSpecs, r)
	if err := s.save(); err != nil {
		return ResolvedToolSpec{}, err
	}
	return r, nil
}

// GetResolvedToolSpecByDigest returns the resolved tool spec with the given ToolSpecDigest.
// Returns ErrNotFound if no such entry exists.
func (s *Store) GetResolvedToolSpecByDigest(toolSpecDigest string) (ResolvedToolSpec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.ResolvedToolSpecs {
		if s.idx.ResolvedToolSpecs[i].ToolSpecDigest == toolSpecDigest {
			return s.idx.ResolvedToolSpecs[i], nil
		}
	}
	return ResolvedToolSpec{}, fmt.Errorf("%w: tool_spec_digest=%q", ErrNotFound, toolSpecDigest)
}

// ListResolvedToolSpecs returns a snapshot of all resolved tool specs.
func (s *Store) ListResolvedToolSpecs() ([]ResolvedToolSpec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]ResolvedToolSpec, len(s.idx.ResolvedToolSpecs))
	copy(out, s.idx.ResolvedToolSpecs)
	return out, nil
}

// ── ToolBuildRecord ───────────────────────────────────────────────────────────

// AppendToolBuildRecord adds a new build record to the index.
// Returns an error if a record with the same BuildID already exists.
//
//nolint:gocritic // hugeParam: ToolBuildRecord by value is intentional — callers own their copy.
func (s *Store) AppendToolBuildRecord(r ToolBuildRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.BuildID == "" {
		return errors.New("index: BuildID must not be empty")
	}
	for i := range s.idx.ToolBuildRecords {
		if s.idx.ToolBuildRecords[i].BuildID == r.BuildID {
			return fmt.Errorf("index: tool build record %q already exists", r.BuildID)
		}
	}
	s.idx.ToolBuildRecords = append(s.idx.ToolBuildRecords, r)
	return s.save()
}

// GetToolBuildRecordByBuildID returns the build record with the given BuildID.
// Returns ErrNotFound if no such record exists.
func (s *Store) GetToolBuildRecordByBuildID(buildID string) (ToolBuildRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.ToolBuildRecords {
		if s.idx.ToolBuildRecords[i].BuildID == buildID {
			return s.idx.ToolBuildRecords[i], nil
		}
	}
	return ToolBuildRecord{}, fmt.Errorf("%w: build_id=%q", ErrNotFound, buildID)
}

// ListToolBuildRecordsByToolSpecDigest returns all build records for the given ToolSpecDigest.
// Returns an empty slice (not an error) if none match.
func (s *Store) ListToolBuildRecordsByToolSpecDigest(toolSpecDigest string) ([]ToolBuildRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []ToolBuildRecord
	for i := range s.idx.ToolBuildRecords {
		if s.idx.ToolBuildRecords[i].ToolSpecDigest == toolSpecDigest {
			out = append(out, s.idx.ToolBuildRecords[i])
		}
	}
	return out, nil
}

// ── ToolImageRecord ───────────────────────────────────────────────────────────

// AppendToolImageRecord adds a new image record to the index.
// Uniqueness is (ImageDigest, BuildID), not ImageDigest alone: a repeat
// build that reproduces the same digest is itself reproducibility
// evidence and must get its own record, not be silently dropped. Returns
// an error only if this exact BuildID already has a record for this digest.
//
//nolint:dupl,gocritic // dupl: same guard+append pattern, distinct types. gocritic: by value is intentional.
func (s *Store) AppendToolImageRecord(r ToolImageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ImageDigest == "" {
		return errors.New("index: ImageDigest must not be empty")
	}
	for i := range s.idx.ToolImageRecords {
		if s.idx.ToolImageRecords[i].ImageDigest == r.ImageDigest && s.idx.ToolImageRecords[i].BuildID == r.BuildID {
			return fmt.Errorf("index: tool image record %q (build_id=%q) already exists", r.ImageDigest, r.BuildID)
		}
	}
	if r.PushedAt.IsZero() {
		r.PushedAt = time.Now().UTC()
	}
	s.idx.ToolImageRecords = append(s.idx.ToolImageRecords, r)
	return s.save()
}

// GetToolImageRecordByDigest returns the image record with the given ImageDigest.
// Returns ErrNotFound if no such record exists.
func (s *Store) GetToolImageRecordByDigest(imageDigest string) (ToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.ToolImageRecords {
		if s.idx.ToolImageRecords[i].ImageDigest == imageDigest {
			return s.idx.ToolImageRecords[i], nil
		}
	}
	return ToolImageRecord{}, fmt.Errorf("%w: image_digest=%q", ErrNotFound, imageDigest)
}

// ListToolImageRecordsByBuildID returns all image records for the given BuildID.
// Returns an empty slice (not an error) if none match.
func (s *Store) ListToolImageRecordsByBuildID(buildID string) ([]ToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []ToolImageRecord
	for i := range s.idx.ToolImageRecords {
		if s.idx.ToolImageRecords[i].BuildID == buildID {
			out = append(out, s.idx.ToolImageRecords[i])
		}
	}
	return out, nil
}

// ── ToolCheckRecord ───────────────────────────────────────────────────────────

// AppendToolCheckRecord stores a new L5-a functional validation result.
// Returns an error if a record with the same CheckID already exists.
//
//nolint:dupl,gocritic // dupl: same guard+append pattern, distinct types. gocritic: by value is intentional.
func (s *Store) AppendToolCheckRecord(r ToolCheckRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.CheckID == "" {
		return errors.New("index: CheckID must not be empty")
	}
	for i := range s.idx.ToolCheckRecords {
		if s.idx.ToolCheckRecords[i].CheckID == r.CheckID {
			return fmt.Errorf("index: tool check record %q already exists", r.CheckID)
		}
	}
	if r.CheckedAt.IsZero() {
		r.CheckedAt = time.Now().UTC()
	}
	s.idx.ToolCheckRecords = append(s.idx.ToolCheckRecords, r)
	return s.save()
}

// GetToolCheckRecordByID returns the check record with the given CheckID.
func (s *Store) GetToolCheckRecordByID(checkID string) (ToolCheckRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.ToolCheckRecords {
		if s.idx.ToolCheckRecords[i].CheckID == checkID {
			return s.idx.ToolCheckRecords[i], nil
		}
	}
	return ToolCheckRecord{}, fmt.Errorf("%w: check_id=%q", ErrNotFound, checkID)
}

// ListToolCheckRecordsByImageDigest returns all check records for the given image digest.
func (s *Store) ListToolCheckRecordsByImageDigest(imageDigest string) ([]ToolCheckRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []ToolCheckRecord
	for i := range s.idx.ToolCheckRecords {
		if s.idx.ToolCheckRecords[i].ImageDigest == imageDigest {
			out = append(out, s.idx.ToolCheckRecords[i])
		}
	}
	return out, nil
}

// ── ToolScanRecord ────────────────────────────────────────────────────────────

// AppendToolScanRecord stores a new L5-b security scan result.
// Returns an error if a record with the same ScanID already exists.
//
//nolint:dupl,gocritic // dupl: same guard+append pattern, distinct types. gocritic: by value is intentional.
func (s *Store) AppendToolScanRecord(r ToolScanRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ScanID == "" {
		return errors.New("index: ScanID must not be empty")
	}
	for i := range s.idx.ToolScanRecords {
		if s.idx.ToolScanRecords[i].ScanID == r.ScanID {
			return fmt.Errorf("index: tool scan record %q already exists", r.ScanID)
		}
	}
	if r.ScannedAt.IsZero() {
		r.ScannedAt = time.Now().UTC()
	}
	s.idx.ToolScanRecords = append(s.idx.ToolScanRecords, r)
	return s.save()
}

// GetToolScanRecordByID returns the scan record with the given ScanID.
func (s *Store) GetToolScanRecordByID(scanID string) (ToolScanRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.ToolScanRecords {
		if s.idx.ToolScanRecords[i].ScanID == scanID {
			return s.idx.ToolScanRecords[i], nil
		}
	}
	return ToolScanRecord{}, fmt.Errorf("%w: scan_id=%q", ErrNotFound, scanID)
}

// ListToolScanRecordsByImageDigest returns all scan records for the given image digest.
func (s *Store) ListToolScanRecordsByImageDigest(imageDigest string) ([]ToolScanRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []ToolScanRecord
	for i := range s.idx.ToolScanRecords {
		if s.idx.ToolScanRecords[i].ImageDigest == imageDigest {
			out = append(out, s.idx.ToolScanRecords[i])
		}
	}
	return out, nil
}

// ── CertifiedToolImageRecord ──────────────────────────────────────────────────

// UpsertCertifiedToolImageRecord creates or replaces a certification record.
// New records are keyed by ToolSpecDigest + Platform; image-digest matching is
// retained only for legacy records which lack either key component.
//
//nolint:dupl,gocritic // dupl: same guard+upsert pattern, distinct types. gocritic: by value is intentional.
func (s *Store) UpsertCertifiedToolImageRecord(r CertifiedToolImageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ImageDigest == "" {
		return errors.New("index: ImageDigest must not be empty")
	}
	if r.CertifiedAt.IsZero() {
		r.CertifiedAt = time.Now().UTC()
	}
	for i := range s.idx.CertifiedToolImageRecords {
		if sameCertifiedToolKey(&s.idx.CertifiedToolImageRecords[i], &r) {
			s.idx.CertifiedToolImageRecords[i] = r
			return s.save()
		}
	}
	s.idx.CertifiedToolImageRecords = append(s.idx.CertifiedToolImageRecords, r)
	return s.save()
}

func sameCertifiedToolKey(a, b *CertifiedToolImageRecord) bool {
	if a.ToolSpecDigest != "" && a.Platform != "" && b.ToolSpecDigest != "" && b.Platform != "" {
		return a.ToolSpecDigest == b.ToolSpecDigest && a.Platform == b.Platform
	}
	return a.ImageDigest == b.ImageDigest
}

// GetCertifiedToolImageRecordByToolSpecDigestAndPlatform returns the current
// certification decision for an explicitly resolved target platform.
func (s *Store) GetCertifiedToolImageRecordByToolSpecDigestAndPlatform(
	toolSpecDigest, platform string,
) (CertifiedToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.idx.CertifiedToolImageRecords {
		rec := s.idx.CertifiedToolImageRecords[i]
		if rec.ToolSpecDigest == toolSpecDigest && rec.Platform == platform {
			return rec, nil
		}
	}
	return CertifiedToolImageRecord{}, fmt.Errorf(
		"%w: tool_spec_digest=%q platform=%q", ErrNotFound, toolSpecDigest, platform)
}

// GetCertifiedToolImageRecord returns the certification record for the given image digest.
func (s *Store) GetCertifiedToolImageRecord(imageDigest string) (CertifiedToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.CertifiedToolImageRecords {
		if s.idx.CertifiedToolImageRecords[i].ImageDigest == imageDigest {
			return s.idx.CertifiedToolImageRecords[i], nil
		}
	}
	return CertifiedToolImageRecord{}, fmt.Errorf("%w: image_digest=%q", ErrNotFound, imageDigest)
}

// ListCertifiedToolImageRecords returns all certification records with the given status.
// Pass "" to return all records.
func (s *Store) ListCertifiedToolImageRecords(status PromotionStatus) ([]CertifiedToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []CertifiedToolImageRecord
	for i := range s.idx.CertifiedToolImageRecords {
		if status == "" || s.idx.CertifiedToolImageRecords[i].PromotionStatus == status {
			out = append(out, s.idx.CertifiedToolImageRecords[i])
		}
	}
	return out, nil
}

// ── ToolFunctionCatalogEntry ──────────────────────────────────────────────────

// UpsertToolFunctionCatalogEntry creates or replaces the catalog entry for the
// given CasHash. Replaces on conflict to allow re-certification updates.
//
//nolint:dupl,gocritic // dupl: same guard+upsert pattern, distinct types. gocritic: by value is intentional.
func (s *Store) UpsertToolFunctionCatalogEntry(e ToolFunctionCatalogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e.CasHash == "" {
		return errors.New("index: CasHash must not be empty")
	}
	if e.CertifiedAt.IsZero() {
		e.CertifiedAt = time.Now().UTC()
	}
	for i := range s.idx.ToolFunctionCatalogEntries {
		if s.idx.ToolFunctionCatalogEntries[i].CasHash == e.CasHash {
			s.idx.ToolFunctionCatalogEntries[i] = e
			return s.save()
		}
	}
	s.idx.ToolFunctionCatalogEntries = append(s.idx.ToolFunctionCatalogEntries, e)
	return s.save()
}

// ListToolFunctionCatalogEntries returns all catalog entries with the given promotion status.
// Pass "" to return all entries.
func (s *Store) ListToolFunctionCatalogEntries(status PromotionStatus) ([]ToolFunctionCatalogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []ToolFunctionCatalogEntry
	for i := range s.idx.ToolFunctionCatalogEntries {
		if status == "" || s.idx.ToolFunctionCatalogEntries[i].PromotionStatus == status {
			out = append(out, s.idx.ToolFunctionCatalogEntries[i])
		}
	}
	return out, nil
}

// ── internal helpers ──────────────────────────────────────────────────────────

func (s *Store) findIndex(casHash string) (int, error) {
	for i := range s.idx.Entries {
		if s.idx.Entries[i].CasHash == casHash {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: cas_hash=%q", ErrNotFound, casHash)
}

// Reload re-reads vault-index.json from disk, replacing the in-memory cache.
// Called by NodePalette before handling each HTTP request to pick up changes
// written by NodeVault (separate process, shared filesystem).
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.idx = &indexFile{SchemaVersion: schemaVersion}
			return nil
		}
		return fmt.Errorf("index: read %s: %w", s.path, err)
	}
	var f indexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("index: parse %s: %w", s.path, err)
	}
	s.idx = &f
	return nil
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal: %w", err)
	}
	//nolint:gosec // path is operator-configured and not from user input
	if err := os.WriteFile(s.path, data, 0o640); err != nil {
		return fmt.Errorf("index: write %s: %w", s.path, err)
	}
	return nil
}
