package index

import (
	"crypto/sha256"
	"encoding/hex"
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
	// schemaVersion 4 adds ValidationRequestRecords.
	// Older files omit these fields; load() treats absent fields as empty slices.
	schemaVersion   = 4
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

// ErrDuplicateRecord is returned by AppendToolCheckRecord(Correlated) and
// AppendToolScanRecord(Correlated) when a record with the same CheckID/
// ScanID already exists. NodeSentinel's CheckID/ScanID are deterministic
// per job (job ID is embedded in both), so a redelivery retry after a
// network failure naturally resubmits the same ID — callers should treat
// this as an idempotent no-op, not a genuine failure.
var ErrDuplicateRecord = errors.New("index: record already exists")

// ErrRecordConflict is returned by AppendToolCheckRecord(Correlated) and
// AppendToolScanRecord(Correlated) when a record with the same CheckID/
// ScanID already exists but its content fingerprint differs — see
// checkRecordFingerprint/scanRecordFingerprint. Unlike ErrDuplicateRecord
// (a safe idempotent redelivery of identical content), this means the same
// ID was reused for a materially different result: silently accepting it
// as "already recorded" would hide whichever content lost the race, and
// certification would keep reflecting a result that no longer matches what
// NodeSentinel most recently reported. Callers must reject this outright —
// no store, no ValidationRequestRecord transition, no re-certification.
var ErrRecordConflict = errors.New("index: record content conflict")

// ErrInvalidTransition is returned by TransitionValidationRequest when the
// record's current status has no allowed edge to the requested one (see
// validValidationTransitions). Callers that call TransitionValidationRequest
// speculatively — e.g. postBuildRegistration's own enqueue-ack Queued
// transition, which can lose a race against a result that already promoted
// the record past Queued — should check errors.Is(err, ErrInvalidTransition)
// to log that as an expected, already-progressed outcome rather than a
// genuine storage failure (a save() error, an I/O error, etc.).
var ErrInvalidTransition = errors.New("index: invalid validation status transition")

// ErrInvalidLifecycleTransition is returned by SetLifecyclePhase when the
// entry's current lifecycle_phase has no allowed edge to the requested one
// (see validLifecycleTransitions). This is deliberately distinct from
// ErrInvalidTransition: pkg/build/service.go treats ErrInvalidTransition as an
// expected, ignorable race on the validation-status axis, so reusing it here
// would let a genuine lifecycle-transition violation be silently swallowed.
var ErrInvalidLifecycleTransition = errors.New("index: invalid lifecycle transition")

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

// validLifecycleTransitions enumerates the only allowed lifecycle_phase edges,
// transcribed directly from docs/PLATFORM_MASTER_DESIGN.md §4.4:
//
//	Pending   → Active                 (L4 pass + RegisterTool)
//	Active    → Retracted              (operator Retract)
//	Retracted → Active | Deleted       (operator Restore | Delete + Harbor GC)
//	Deleted   → ∅                      (terminal)
//
// Active → Deleted is forbidden — Retracted must be traversed first. Any edge
// not listed here (including self-edges and every Deleted → * edge) is rejected
// by SetLifecyclePhase. The Retracted → Active (Restore) edge is present because
// §4.4 defines it, but no caller drives it yet: the store has no RestoreTool
// path (tracked separately as F-3c).
var validLifecycleTransitions = map[LifecyclePhase][]LifecyclePhase{
	PhasePending:   {PhaseActive},
	PhaseActive:    {PhaseRetracted},
	PhaseRetracted: {PhaseActive, PhaseDeleted},
	PhaseDeleted:   {},
}

// SetLifecyclePhase updates the lifecycle_phase of the entry identified by casHash.
//
// The transition must be an allowed edge in validLifecycleTransitions (§4.4);
// otherwise the entry is left untouched and ErrInvalidLifecycleTransition is
// returned. On rejection no save() occurs, so the on-disk index is unchanged.
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
	current := s.idx.Entries[idx].LifecyclePhase
	allowed := false
	for _, next := range validLifecycleTransitions[current] {
		if next == phase {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidLifecycleTransition, current, phase)
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

// GetLatestToolImageRecordByRef returns the most recently pushed ToolImageRecord
// whose ImageRef equals ref (multiple builds can share the same tag — e.g. a
// version tag reused across rebuilds, or :latest across any tool version).
// Returns ErrNotFound if no record has this ref.
func (s *Store) GetLatestToolImageRecordByRef(ref string) (ToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latest ToolImageRecord
	found := false
	for i := range s.idx.ToolImageRecords {
		if s.idx.ToolImageRecords[i].ImageRef != ref {
			continue
		}
		if !found || s.idx.ToolImageRecords[i].PushedAt.After(latest.PushedAt) {
			latest = s.idx.ToolImageRecords[i]
			found = true
		}
	}
	if !found {
		return ToolImageRecord{}, fmt.Errorf("%w: image_ref=%q", ErrNotFound, ref)
	}
	return latest, nil
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

	if err := s.appendToolCheckRecordLocked(r); err != nil {
		return err
	}
	return s.save()
}

// AppendToolCheckRecordCorrelated stores a ToolCheckRecord and, in the same
// locked critical section (one save() covering both), updates the
// ValidationRequestRecord identified by validationRequestID: promotes
// Queued -> Running (setting SentinelJobID if given), and — only when
// terminal is true — closes it out to Succeeded (succeeded=true) or Failed
// (succeeded=false, recording failureReason). This is what keeps "the
// record was stored" and "the correlation status reflects it" from ever
// observably diverging (see review guidance on PR2-B: record save and
// ValidationRequestRecord status update must be atomic).
//
// validationRequestID may be empty or unknown to this store (no matching
// ValidationRequestRecord) — both are silent no-ops here, matching the
// fail-open correlation policy applied by the caller (pkg/catalogrest)
// before this is ever invoked: a missing/orphan ID must not block storing
// the record itself, only a *found* record with a mismatched image digest
// does (and that rejection happens earlier, before this call).
//
//nolint:gocritic // hugeParam: ToolCheckRecord by value is intentional — callers own their copy.
func (s *Store) AppendToolCheckRecordCorrelated(
	r ToolCheckRecord, validationRequestID, sentinelJobID string, terminal, succeeded bool, failureReason string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.appendToolCheckRecordLocked(r); err != nil {
		return err
	}
	s.applyValidationCorrelationLocked(validationRequestID, sentinelJobID, terminal, succeeded, failureReason)
	return s.save()
}

// dupl: mirrors appendToolScanRecordLocked, distinct types. gocritic: by-value is intentional.
//
//nolint:dupl,gocritic
func (s *Store) appendToolCheckRecordLocked(r ToolCheckRecord) error {
	if r.CheckID == "" {
		return errors.New("index: CheckID must not be empty")
	}
	for i := range s.idx.ToolCheckRecords {
		if s.idx.ToolCheckRecords[i].CheckID == r.CheckID {
			existing, incoming := checkRecordFingerprint(s.idx.ToolCheckRecords[i]), checkRecordFingerprint(r)
			if existing != incoming {
				return fmt.Errorf("%w: check_id=%q existing_fingerprint=%s incoming_fingerprint=%s",
					ErrRecordConflict, r.CheckID, existing[:12], incoming[:12])
			}
			return fmt.Errorf("%w: check_id=%q", ErrDuplicateRecord, r.CheckID)
		}
	}
	if r.CheckedAt.IsZero() {
		r.CheckedAt = time.Now().UTC()
	}
	s.idx.ToolCheckRecords = append(s.idx.ToolCheckRecords, r)
	return nil
}

// checkRecordFingerprintFields is the semantic subset of ToolCheckRecord
// that identifies "the same validation fact reported twice" — deliberately
// excludes CheckID (the key being compared, not content) and CheckedAt
// (a transport/receipt timestamp, not a fact about the validation itself).
// ValidationRequestID/SentinelJobID ARE included: they're correlation
// facts, not transport metadata — a redelivery of the same result must
// still carry the same correlation, and a genuine change of either means
// this is not the same report.
type checkRecordFingerprintFields struct {
	ToolSpecDigest          string
	ImageDigest             string
	Platform                string
	ToolName                string
	Version                 string
	ValidationRequestID     string
	SentinelJobID           string
	Stage                   string
	Terminal                bool
	ValidationStatus        string
	ValidationHash          string
	Command                 string
	ExitCode                int
	ObservedIoProfile       *ObservedIoProfile
	ObservedResourceProfile *ObservedResourceProfile
	ContractCheck           *ContractCheck
	FailureKind             string
	FailureCode             string
	Retryable               bool
	FailureReason           string
}

// checkRecordFingerprint hashes the semantic content of r (see
// checkRecordFingerprintFields) so appendToolCheckRecordLocked can tell a
// safe idempotent redelivery (same CheckID, same fingerprint ->
// ErrDuplicateRecord) apart from the same CheckID reused for a materially
// different result (different fingerprint -> ErrRecordConflict). Uses
// json.Marshal's deterministic field-order encoding of a fixed Go struct —
// this only ever compares two hashes produced by this same function in this
// same process, so canonical-across-languages encoding isn't a concern.
//
//nolint:gocritic // hugeParam: ToolCheckRecord by value is intentional — read-only.
func checkRecordFingerprint(r ToolCheckRecord) string {
	fields := checkRecordFingerprintFields{
		ToolSpecDigest:          r.ToolSpecDigest,
		ImageDigest:             r.ImageDigest,
		Platform:                r.Platform,
		ToolName:                r.ToolName,
		Version:                 r.Version,
		ValidationRequestID:     r.ValidationRequestID,
		SentinelJobID:           r.SentinelJobID,
		Stage:                   r.Stage,
		Terminal:                r.Terminal,
		ValidationStatus:        r.ValidationStatus,
		ValidationHash:          r.ValidationHash,
		Command:                 r.Command,
		ExitCode:                r.ExitCode,
		ObservedIoProfile:       r.ObservedIoProfile,
		ObservedResourceProfile: r.ObservedResourceProfile,
		ContractCheck:           r.ContractCheck,
		FailureKind:             r.FailureKind,
		FailureCode:             r.FailureCode,
		Retryable:               r.Retryable,
		FailureReason:           r.FailureReason,
	}
	// json.Marshal on a fixed struct type never fails (no channels/funcs/
	// cyclic data among these field types), so the error is unreachable.
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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

	if err := s.appendToolScanRecordLocked(r); err != nil {
		return err
	}
	return s.save()
}

// AppendToolScanRecordCorrelated is AppendToolScanRecord's counterpart to
// AppendToolCheckRecordCorrelated — see that method's doc comment. A scan
// record has no ValidationStatus of its own; succeeded is the caller's
// verdict derived from PolicyResult (e.g. "blocked" -> false).
//
//nolint:gocritic // hugeParam: ToolScanRecord by value is intentional — callers own their copy.
func (s *Store) AppendToolScanRecordCorrelated(
	r ToolScanRecord, validationRequestID, sentinelJobID string, terminal, succeeded bool,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.appendToolScanRecordLocked(r); err != nil {
		return err
	}
	s.applyValidationCorrelationLocked(validationRequestID, sentinelJobID, terminal, succeeded, "")
	return s.save()
}

//nolint:dupl,gocritic // dupl: same guard+append pattern, distinct types. gocritic: by value is intentional.
func (s *Store) appendToolScanRecordLocked(r ToolScanRecord) error {
	if r.ScanID == "" {
		return errors.New("index: ScanID must not be empty")
	}
	for i := range s.idx.ToolScanRecords {
		if s.idx.ToolScanRecords[i].ScanID == r.ScanID {
			existing, incoming := scanRecordFingerprint(s.idx.ToolScanRecords[i]), scanRecordFingerprint(r)
			if existing != incoming {
				return fmt.Errorf("%w: scan_id=%q existing_fingerprint=%s incoming_fingerprint=%s",
					ErrRecordConflict, r.ScanID, existing[:12], incoming[:12])
			}
			return fmt.Errorf("%w: scan_id=%q", ErrDuplicateRecord, r.ScanID)
		}
	}
	if r.ScannedAt.IsZero() {
		r.ScannedAt = time.Now().UTC()
	}
	s.idx.ToolScanRecords = append(s.idx.ToolScanRecords, r)
	return nil
}

// scanRecordFingerprintFields/scanRecordFingerprint mirror
// checkRecordFingerprintFields/checkRecordFingerprint — see those doc
// comments. Excludes ScanID (the key) and ScannedAt (a receipt timestamp).
type scanRecordFingerprintFields struct {
	ImageDigest         string
	ToolName            string
	Platform            string
	ValidationRequestID string
	SentinelJobID       string
	Stage               string
	Terminal            bool
	Scanner             string
	ScannerVersion      string
	DbDigest            string
	Source              string
	CriticalCount       int
	HighCount           int
	MediumCount         int
	LowCount            int
	PolicyMode          string
	PolicyResult        string
}

//nolint:gocritic // hugeParam: ToolScanRecord by value is intentional — read-only.
func scanRecordFingerprint(r ToolScanRecord) string {
	fields := scanRecordFingerprintFields{
		ImageDigest:         r.ImageDigest,
		ToolName:            r.ToolName,
		Platform:            r.Platform,
		ValidationRequestID: r.ValidationRequestID,
		SentinelJobID:       r.SentinelJobID,
		Stage:               r.Stage,
		Terminal:            r.Terminal,
		Scanner:             r.Scanner,
		ScannerVersion:      r.ScannerVersion,
		DbDigest:            r.DbDigest,
		Source:              r.Source,
		CriticalCount:       r.CriticalCount,
		HighCount:           r.HighCount,
		MediumCount:         r.MediumCount,
		LowCount:            r.LowCount,
		PolicyMode:          r.PolicyMode,
		PolicyResult:        r.PolicyResult,
	}
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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

// ── ValidationRequestRecord ───────────────────────────────────────────────────

// CreateValidationRequestRecord durably records a new logical validation
// request in EnqueuePending status, before NodeVault calls NodeSentinel.
// Returns an error if a record with the same ValidationRequestID already
// exists — callers must mint a fresh ValidationRequestID per logical
// request (see pkg/build's validation request ID generation) and only reuse
// one to retry the exact same request after a transport/process failure.
//
//nolint:gocritic // hugeParam: ValidationRequestRecord by value is intentional — callers own their copy.
func (s *Store) CreateValidationRequestRecord(r ValidationRequestRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ValidationRequestID == "" {
		return errors.New("index: ValidationRequestID must not be empty")
	}
	for i := range s.idx.ValidationRequestRecords {
		if s.idx.ValidationRequestRecords[i].ValidationRequestID == r.ValidationRequestID {
			return fmt.Errorf("index: validation request record %q already exists", r.ValidationRequestID)
		}
	}
	if r.RequestedAt.IsZero() {
		r.RequestedAt = time.Now().UTC()
	}
	if r.ValidationStatus == "" {
		r.ValidationStatus = ValidationEnqueuePending
	}
	s.idx.ValidationRequestRecords = append(s.idx.ValidationRequestRecords, r)
	return s.save()
}

// GetValidationRequestRecord returns the record with the given ValidationRequestID.
// Returns ErrNotFound if no such record exists.
func (s *Store) GetValidationRequestRecord(validationRequestID string) (ValidationRequestRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.idx.ValidationRequestRecords {
		if s.idx.ValidationRequestRecords[i].ValidationRequestID == validationRequestID {
			return s.idx.ValidationRequestRecords[i], nil
		}
	}
	return ValidationRequestRecord{}, fmt.Errorf("%w: validation_request_id=%q", ErrNotFound, validationRequestID)
}

// ListValidationRequestsByStatus returns copies of every ValidationRequestRecord
// currently in the given status. The enqueue-retry loop uses it to find requests
// stuck in ValidationUnavailable. RequestedActions is deep-copied so a caller can
// rebuild an EnqueueValidationWorkRequest from the result without aliasing store
// state. The error return mirrors the other List* accessors; it is always nil.
func (s *Store) ListValidationRequestsByStatus(status ValidationStatus) ([]ValidationRequestRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]ValidationRequestRecord, 0, len(s.idx.ValidationRequestRecords))
	for i := range s.idx.ValidationRequestRecords {
		if s.idx.ValidationRequestRecords[i].ValidationStatus != status {
			continue
		}
		rec := s.idx.ValidationRequestRecords[i]
		if rec.RequestedActions != nil {
			rec.RequestedActions = append([]string(nil), rec.RequestedActions...)
		}
		out = append(out, rec)
	}
	return out, nil
}

// validValidationTransitions enumerates the only allowed
// ValidationStatus -> ValidationStatus edges — see ValidationStatus's doc
// comment for the full state graph. TransitionValidationRequest rejects
// any edge not listed here (e.g. Succeeded -> Running, Failed -> Queued),
// so a caller applying a stale/out-of-order update cannot silently corrupt
// a record that has already reached a terminal or in-progress status.
// EnqueuePending -> Running exists for a real race, not as a design nicety:
// NodeSentinel can execute a job and submit its result before NodeVault's
// own postBuildRegistration has processed the enqueue response and driven
// this record to Queued. Without this edge, a result arriving that early
// would have both its Running promotion and any terminal transition
// silently rejected (see applyValidationCorrelationLocked), orphaning the
// record at EnqueuePending forever. Once here, forward progress is
// one-way: Queued has no edge sourced from Running/Succeeded/Failed, so a
// late-arriving enqueue ACK's own Queued transition attempt is rejected by
// this same graph instead of regressing a record that already moved on.
var validValidationTransitions = map[ValidationStatus][]ValidationStatus{
	ValidationEnqueuePending: {ValidationQueued, ValidationUnavailable, ValidationRunning},
	ValidationUnavailable:    {ValidationEnqueuePending, ValidationEnqueueAbandoned},
	ValidationQueued:         {ValidationRunning, ValidationFailed, ValidationInterrupted},
	ValidationRunning:        {ValidationSucceeded, ValidationFailed, ValidationInterrupted},
}

// TransitionValidationRequest moves the record identified by validationRequestID
// from its current status to `to`, applying mutate (which may be nil) to the
// record before the status itself is updated and saved. Returns an error if
// the record does not exist, or if the current status has no allowed edge to
// `to` in validValidationTransitions.
func (s *Store) TransitionValidationRequest(
	validationRequestID string, to ValidationStatus, mutate func(*ValidationRequestRecord),
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := s.findValidationRequestIndex(validationRequestID)
	if idx == -1 {
		return fmt.Errorf("%w: validation_request_id=%q", ErrNotFound, validationRequestID)
	}
	if err := s.transitionValidationRequestLocked(idx, to, mutate); err != nil {
		return fmt.Errorf("%w for %q", err, validationRequestID)
	}
	return s.save()
}

func (s *Store) findValidationRequestIndex(validationRequestID string) int {
	for i := range s.idx.ValidationRequestRecords {
		if s.idx.ValidationRequestRecords[i].ValidationRequestID == validationRequestID {
			return i
		}
	}
	return -1
}

// transitionValidationRequestLocked applies the from-current-status ->
// `to` edge check and mutate callback for the record at s.idx.
// ValidationRequestRecords[idx], without saving — callers hold s.mu and
// call s.save() themselves once, possibly after other locked writes in the
// same critical section (see AppendToolCheckRecordCorrelated).
func (s *Store) transitionValidationRequestLocked(
	idx int, to ValidationStatus, mutate func(*ValidationRequestRecord),
) error {
	current := s.idx.ValidationRequestRecords[idx].ValidationStatus
	allowed := false
	for _, next := range validValidationTransitions[current] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current, to)
	}

	if mutate != nil {
		mutate(&s.idx.ValidationRequestRecords[idx])
	}
	s.idx.ValidationRequestRecords[idx].ValidationStatus = to
	return nil
}

// applyValidationCorrelationLocked applies a validation result's
// correlation side effects to the ValidationRequestRecord identified by
// validationRequestID: promotes Queued -> Running (setting SentinelJobID if
// given), then — only if terminal — closes it out to Succeeded/Failed. Must
// be called with s.mu already held; does not call save().
//
// validationRequestID empty or not found: silent no-op — this is the
// fail-open path for a missing/orphan ID (see AppendToolCheckRecordCorrelated's
// doc comment); a *found* record with a mismatched image digest must be
// rejected by the caller before ever reaching this method, not handled here.
//
// Any transition failure here (e.g. this result arrives after the request
// already reached a terminal status — a duplicate/late redelivery) is
// swallowed, not returned: a duplicate or out-of-order correlation update
// must never fail the record append that already committed in the same
// call.
func (s *Store) applyValidationCorrelationLocked(
	validationRequestID, sentinelJobID string, terminal, succeeded bool, failureReason string,
) {
	if validationRequestID == "" {
		return
	}
	idx := s.findValidationRequestIndex(validationRequestID)
	if idx == -1 {
		return
	}

	_ = s.transitionValidationRequestLocked(idx, ValidationRunning, func(r *ValidationRequestRecord) {
		if sentinelJobID != "" {
			r.SentinelJobID = sentinelJobID
		}
	})

	if !terminal {
		return
	}
	target := ValidationSucceeded
	if !succeeded {
		target = ValidationFailed
	}
	_ = s.transitionValidationRequestLocked(idx, target, func(r *ValidationRequestRecord) {
		r.CompletedAt = time.Now().UTC()
		if !succeeded {
			r.FailureReason = failureReason
		}
	})
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
