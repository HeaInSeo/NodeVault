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
	// schemaVersion 5 adds RegisteredToolFunctions,
	// ToolFunctionPresentationRevisions, and ToolFunctionRequestRecords
	// (issue #19 W2 RegisterToolFunction).
	// schemaVersion 6 adds ResolvedToolSpec.RawSpecSchemaVersion and
	// ResolvedToolSpec.DerivationVersion (W3-PRE raw_spec schema authority). The bump makes
	// this durable provenance actually durable: a rolled-back schema-5 binary refuses the file
	// via load()'s version guard instead of silently dropping the provenance fields on its
	// next save (which would defeat the frozen-derivation guarantee).
	// Older files omit these fields; load() treats absent fields as empty slices.
	//
	// Every bump so far has been purely additive (new optional fields/sections
	// only), so load() accepts any f.SchemaVersion <= schemaVersion without a
	// migration step — see the guard in load(). If a future bump ever
	// removes/renames/reinterprets a field instead of only adding one, this
	// assumption breaks and load() must gain real per-version migration
	// logic, not just a version check.
	schemaVersion   = 6
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
	// toolFunctionDurabilityUncertain is set when a RegisterToolFunctionAtomic save reached
	// os.Rename but a subsequent parent-dir fsync failed (errIndexPersistedNotDurable): the
	// record is on disk and in memory, but the rename's directory entry may not survive a
	// crash. It is cleared only by a fully-successful save() (which fsyncs the dir, making
	// prior renames durable). RegisterToolFunctionAtomic re-saves before acknowledging ANY
	// result (including an idempotent replay) while this is set, so no success is returned
	// on an un-fsync'd rename. Guarded by mu.
	toolFunctionDurabilityUncertain bool
}

// ErrNotFound is returned when a requested entry does not exist.
var ErrNotFound = errors.New("index: entry not found")

// ErrEntryExists is returned by Append when an entry with the same CasHash
// already exists. Because casHash is a pure content identity (constitution
// §1.2), a byte-identical re-registration produces the same casHash and reaches
// this path. Callers should treat it as an idempotent no-op and return the
// existing entry's authoritative lifecycle/integrity state rather than a freshly
// fabricated one (see catalog.RegisterTool / RegisterData).
var ErrEntryExists = errors.New("index: entry already exists")

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

// ErrToolFunctionRequestConflict is returned by RegisterToolFunctionAtomic when a
// request_id was already used for a registration that resolved to a DIFFERENT
// runnable CasHash. RegisterToolFunction is idempotent by request_id: the same
// request_id replaying identical content reconciles to the same record, but reusing
// it for materially different content (a different resulting CasHash) is a conflict
// — the store rejects it fail-closed with no mutation rather than silently
// overwriting or forking the earlier request's result.
var ErrToolFunctionRequestConflict = errors.New("index: tool function request id content conflict")

// ErrInvalidLifecycleTransition is returned by SetLifecyclePhase when the
// entry's current lifecycle_phase has no allowed edge to the requested one
// (see validLifecycleTransitions). This is deliberately distinct from
// ErrInvalidTransition: pkg/build/service.go treats ErrInvalidTransition as an
// expected, ignorable race on the validation-status axis, so reusing it here
// would let a genuine lifecycle-transition violation be silently swallowed.
var ErrInvalidLifecycleTransition = errors.New("index: invalid lifecycle transition")

// errIndexPersistedNotDurable tags a save() failure that occurred AFTER os.Rename had
// already atomically swapped the replacement index into place, i.e. a parent-directory
// fsync/close error. The on-disk index already reflects the write, so a caller that
// mutated memory before save() MUST NOT roll that mutation back on this error — memory
// already matches disk, and rolling back would diverge them and let a later save()
// overwrite the committed file with the stale index. The error is still propagated so the
// operation surfaces the durability uncertainty (the rename may not survive a power loss
// until the dir entry is fsync'd) and the caller can retry. Pre-rename save() errors do
// NOT carry this tag, so their callers still roll back (disk is unchanged there).
var errIndexPersistedNotDurable = errors.New("index: persisted but parent-dir fsync failed (durability uncertain)")

// failAfterRenameForTest is a test-only injection point (nil in production) invoked in
// save() immediately after a successful os.Rename to simulate a post-rename durability
// failure. See save().
var failAfterRenameForTest func() error

// syncDirForTest overrides syncDir when non-nil (nil in production). Test-only: lets a test
// observe that the startup/save directory fsync is invoked and simulate its failure.
var syncDirForTest func(string) error

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
	// Conservative startup durability: fsync the index parent directory before the Store is
	// usable, so any directory entry (e.g. an index rename) that was visible but not yet
	// fsync'd when a prior process crashed becomes durable now. Fail closed — a Store whose
	// startup durability cannot be established must not serve/ack requests (W2 round-2 P1).
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("index: startup durability sync failed: %w", err)
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
	// See New: conservative startup parent-directory durability sync, fail-closed.
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("index: startup durability sync failed: %w", err)
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
			return fmt.Errorf("%w: cas_hash=%q", ErrEntryExists, e.CasHash)
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

// ListByStableRef returns all entries with the given stableRef, regardless of
// lifecycle_phase. It intentionally does NOT enforce the Active-only Catalog
// exposure rule, and as of gap #19 it has no production callers —
// pkg/certification now correlates by ImageDigest (GetByImageDigest). Any
// caller that serves results to an external client (REST/gRPC listing) must
// use ListActiveByStableRef instead.
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

// ListActiveByStableRef returns entries matching stableRef AND
// lifecycle_phase == Active — the Catalog-exposure-safe counterpart to
// ListByStableRef. Use this (not ListByStableRef) for any REST/gRPC listing
// endpoint that narrows ListActive's results by stable_ref, so a
// Retracted/Deleted/Pending entry is never exposed just because the caller
// happened to query by stable_ref instead of listing everything.
func (s *Store) ListActiveByStableRef(stableRef string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Entry
	for i := range s.idx.Entries {
		if s.idx.Entries[i].StableRef == stableRef && s.idx.Entries[i].LifecyclePhase == PhaseActive {
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

// CompareAndSetIntegrityHealth updates the integrity_health of the entry identified by
// casHash to newValue, but only if its current integrity_health still equals
// expectedCurrent. This closes a stale-snapshot race: a long-running check (e.g. the
// slow reconcile loop's pull-reachability probe) may snapshot an entry's state, and by
// the time it is ready to write its verdict, a concurrent fast-loop pass may have
// already moved the entry to a fresher state. Writing unconditionally would clobber
// that fresher verdict with a stale one.
//
// swapped reports whether the write was performed. If the entry's integrity_health no
// longer matches expectedCurrent, the write is skipped and swapped is false — this is
// not an error; a fresher value already won.
//
// IMPORTANT: This method MUST be called only by the reconcile loop, same as
// SetIntegrityHealth.
func (s *Store) CompareAndSetIntegrityHealth(
	casHash string, expectedCurrent, newValue IntegrityHealth,
) (swapped bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, err := s.findIndex(casHash)
	if err != nil {
		return false, err
	}
	if s.idx.Entries[idx].IntegrityHealth != expectedCurrent {
		return false, nil
	}
	s.idx.Entries[idx].IntegrityHealth = newValue
	s.idx.Entries[idx].HealthCheckedAt = time.Now().UTC()
	if err := s.save(); err != nil {
		return false, err
	}
	return true, nil
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
	if err := s.save(); err != nil {
		// Roll back the in-memory append so a failed durable write does not leave a
		// phantom record that a later lookup (e.g. GetLatestToolImageRecordByDigest)
		// could treat as an authoritative digest->locator mapping. save() writes
		// atomically (tmp + rename), so the on-disk index is unchanged on failure.
		s.idx.ToolImageRecords = s.idx.ToolImageRecords[:len(s.idx.ToolImageRecords)-1]
		return err
	}
	return nil
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

// GetLatestToolImageRecordByDigest returns the most recently pushed
// ToolImageRecord with the given ImageDigest. The store permits the same digest
// across multiple build IDs (the same image content recorded by successive builds,
// possibly under different locators); the earliest such record can point at a
// repository whose manifest has since been garbage-collected, so a caller that
// needs a live locator to pull from must prefer the most recent record rather than
// the first inserted (which GetToolImageRecordByDigest returns). Returns
// ErrNotFound if no record has this digest.
func (s *Store) GetLatestToolImageRecordByDigest(imageDigest string) (ToolImageRecord, error) {
	return s.latestToolImageRecord(
		func(r ToolImageRecord) bool { return r.ImageDigest == imageDigest },
		"image_digest", imageDigest)
}

// GetLatestToolImageRecordByRef returns the most recently pushed ToolImageRecord
// whose ImageRef equals ref (multiple builds can share the same tag — e.g. a
// version tag reused across rebuilds, or :latest across any tool version).
// Returns ErrNotFound if no record has this ref.
func (s *Store) GetLatestToolImageRecordByRef(ref string) (ToolImageRecord, error) {
	return s.latestToolImageRecord(
		func(r ToolImageRecord) bool { return r.ImageRef == ref },
		"image_ref", ref)
}

// latestToolImageRecord returns the most recently pushed (by PushedAt)
// ToolImageRecord matching pred, or ErrNotFound (qualified by keyName=keyVal) if
// none match. Shared by the digest- and ref-keyed latest-record lookups.
func (s *Store) latestToolImageRecord(
	pred func(ToolImageRecord) bool, keyName, keyVal string,
) (ToolImageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latest ToolImageRecord
	found := false
	for i := range s.idx.ToolImageRecords {
		if !pred(s.idx.ToolImageRecords[i]) {
			continue
		}
		if !found || s.idx.ToolImageRecords[i].PushedAt.After(latest.PushedAt) {
			latest = s.idx.ToolImageRecords[i]
			found = true
		}
	}
	if !found {
		return ToolImageRecord{}, fmt.Errorf("%w: %s=%q", ErrNotFound, keyName, keyVal)
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
//
// It filters on the promotion_status axis only and never consults
// lifecycle_phase, so it is NOT exposure-safe: an entry whose index entry has
// been Retracted or Deleted is still returned, as is one that has no index
// entry at all. Any caller serving results to an external client (REST/gRPC
// certified-tools) must use ListActiveToolFunctionCatalogEntries instead.
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

// ListActiveToolFunctionCatalogEntries returns catalog entries with the given
// promotion status whose backing index entry is in lifecycle_phase Active —
// the Catalog-exposure-safe counterpart to ListToolFunctionCatalogEntries.
//
// lifecycle_phase and promotion_status are independent axes
// (docs/PLATFORM_MASTER_DESIGN.md §4.4, §4.5): retracting a tool moves
// lifecycle_phase to Retracted and deliberately leaves promotion_status alone,
// so a promotion_status-only filter keeps serving retracted tools. Here
// lifecycle_phase is the outer gate that decides whether the artifact exists
// for an external caller at all, and the promotion status predicate is the
// inner filter applied within the Active set. Pass "" to skip the inner filter.
//
// Fail-closed: a catalog entry whose CasHash has no Active index entry is
// excluded, including one with no index entry whatsoever.
func (s *Store) ListActiveToolFunctionCatalogEntries(status PromotionStatus) ([]ToolFunctionCatalogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Join under a single RLock: collect the Active CasHash set once (O(n)),
	// then filter the catalog entries against it (O(m)). Calling GetByCasHash
	// per catalog entry would re-acquire s.mu recursively and be O(n*m).
	active := make(map[string]struct{}, len(s.idx.Entries))
	for i := range s.idx.Entries {
		if s.idx.Entries[i].LifecyclePhase == PhaseActive {
			active[s.idx.Entries[i].CasHash] = struct{}{}
		}
	}

	var out []ToolFunctionCatalogEntry
	for i := range s.idx.ToolFunctionCatalogEntries {
		e := &s.idx.ToolFunctionCatalogEntries[i]
		if _, ok := active[e.CasHash]; !ok {
			continue
		}
		if status == "" || e.PromotionStatus == status {
			out = append(out, *e)
		}
	}
	return out, nil
}

// GetActiveToolFunctionCatalogEntry returns the catalog entry for casHash,
// gated on its index entry being in lifecycle_phase Active. Returns ErrNotFound
// when no catalog entry exists for casHash, when its index entry is missing
// (fail-closed), or when that index entry is not Active.
//
// No promotion_status filter is applied, preserving the existing GET semantics:
// lifecycle_phase alone decides whether the entry exists for an external
// caller, and promotion_status is reported back to that caller as data, so a
// superseded-but-Active tool stays fetchable by hash.
func (s *Store) GetActiveToolFunctionCatalogEntry(casHash string) (ToolFunctionCatalogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Single RLock for both halves of the join — a GetByCasHash call here would
	// re-acquire s.mu on a non-reentrant RWMutex. Matching on phase as well as
	// CasHash keeps this identical to the any-Active-entry-wins rule
	// ListActiveToolFunctionCatalogEntries applies.
	activeIndexEntry := false
	for i := range s.idx.Entries {
		if s.idx.Entries[i].CasHash == casHash && s.idx.Entries[i].LifecyclePhase == PhaseActive {
			activeIndexEntry = true
			break
		}
	}
	if !activeIndexEntry {
		return ToolFunctionCatalogEntry{}, fmt.Errorf("%w: cas_hash=%q", ErrNotFound, casHash)
	}

	for i := range s.idx.ToolFunctionCatalogEntries {
		if s.idx.ToolFunctionCatalogEntries[i].CasHash == casHash {
			return s.idx.ToolFunctionCatalogEntries[i], nil
		}
	}
	return ToolFunctionCatalogEntry{}, fmt.Errorf("%w: cas_hash=%q", ErrNotFound, casHash)
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
//
// Unavailable -> Queued is the enqueue-retry loop's success edge: it retries
// while the record is still Unavailable (never persisting an intermediate
// EnqueuePending, so a crash mid-retry cannot strand the record — see
// pkg/reconcile.EnqueueRetrier.retryOne) and, on a successful re-enqueue,
// moves straight to Queued. Unavailable -> EnqueuePending remains for a
// manual/future re-arm path.
//
// Unavailable -> Running is the same early-result race as EnqueuePending ->
// Running, reachable because the enqueue-retry loop also recovers a request
// stranded in EnqueuePending by marking it Unavailable (see
// pkg/reconcile.EnqueueRetrier.recoverStrandedPending). If that record's
// original enqueue had actually reached NodeSentinel before the crash, its job
// is already running, so a result can arrive while the record sits Unavailable
// awaiting re-send. Without this edge that result's correlation would be
// silently rejected (see applyValidationCorrelationLocked), orphaning the record
// at Unavailable forever. This edge is a correlation path only; the retry loop
// never transitions to Running, so its retry/backoff/escalation flow is
// unaffected. It does not weaken rejection of genuine transport failures either:
// a request that truly never reached NodeSentinel has no job and so no result
// ever arrives to exercise the edge.
var validValidationTransitions = map[ValidationStatus][]ValidationStatus{
	ValidationEnqueuePending: {ValidationQueued, ValidationUnavailable, ValidationRunning},
	ValidationUnavailable:    {ValidationEnqueuePending, ValidationQueued, ValidationRunning, ValidationEnqueueAbandoned},
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

// UpdateValidationRequestRetryState updates the retry-bookkeeping fields of an
// Unavailable request in place, without a status change. The enqueue-retry loop
// uses it to record a failed retry's incremented attempt count, backed-off
// NextAttemptAt, and last error while the record stays Unavailable — so a crash
// mid-retry leaves it exactly where RetryDue will find it again, never stranded
// in EnqueuePending. It errors (ErrInvalidTransition) if the record is not
// currently Unavailable, and pins the status across mutate, so it cannot be
// used to smuggle a record around validValidationTransitions.
func (s *Store) UpdateValidationRequestRetryState(
	validationRequestID string, mutate func(*ValidationRequestRecord),
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := s.findValidationRequestIndex(validationRequestID)
	if idx == -1 {
		return fmt.Errorf("%w: validation_request_id=%q", ErrNotFound, validationRequestID)
	}
	current := s.idx.ValidationRequestRecords[idx].ValidationStatus
	if current != ValidationUnavailable {
		return fmt.Errorf("%w: retry-state update requires Unavailable, got %s for %q",
			ErrInvalidTransition, current, validationRequestID)
	}
	if mutate != nil {
		mutate(&s.idx.ValidationRequestRecords[idx])
		// Pin the status: this method is bookkeeping only, never a transition.
		s.idx.ValidationRequestRecords[idx].ValidationStatus = current
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
// validationRequestID: promotes the record to Running (setting SentinelJobID if
// given), then — only if terminal — closes it out to Succeeded/Failed. The
// promotion is valid from Queued, from EnqueuePending, or from Unavailable (all
// three have a Running edge in validValidationTransitions), so an early result —
// including one for a request the enqueue-retry loop recovered to Unavailable —
// correlates rather than being orphaned. Must be called with s.mu already held;
// does not call save().
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

// ── RegisteredToolFunction (issue #19 W2) ─────────────────────────────────────

// RegisterToolFunctionAtomic durably registers a runnable ToolFunction, its
// optional presentation revision, and (when reqID != "") its request-id record in
// a single atomic save(). It is idempotent along two independent axes and never
// overwrites an existing runnable record:
//
//   - request_id: if reqID was already used, a replay resolving to the SAME CasHash
//     returns the existing record unchanged; a replay resolving to a DIFFERENT
//     CasHash is rejected with ErrToolFunctionRequestConflict (no mutation).
//   - content (CasHash): if the CasHash already exists (same content via a prior
//     request), the existing record is returned as-is — its authoritative
//     LifecyclePhase is preserved, never resurrected to Active and never
//     overwritten — and no new presentation revision is created. A new reqID that
//     first observes existing content is still recorded so its later replays are
//     idempotent.
//
// created is true only when a brand-new runnable record was appended.
//
//nolint:gocritic // hugeParam: RegisteredToolFunction by value is intentional — callers own their copy.
func (s *Store) RegisterToolFunctionAtomic(
	reqID string, rec RegisteredToolFunction, rev *ToolFunctionPresentationRevision,
) (stored RegisteredToolFunction, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Durability repair: if a prior registration reached os.Rename but its parent-dir fsync
	// failed, the record is on disk/in memory but the rename may not survive a crash. Re-save
	// (which re-runs the dir fsync) BEFORE acknowledging any result here — including an
	// idempotent replay that would otherwise return success without touching disk — so no
	// success is ever returned on an un-fsync'd rename. A still-failing re-save keeps the
	// flag set and surfaces the durability error for another retry.
	if s.toolFunctionDurabilityUncertain {
		if serr := s.save(); serr != nil {
			return RegisteredToolFunction{}, false, serr
		}
		s.toolFunctionDurabilityUncertain = false
	}

	if rec.CasHash == "" {
		return RegisteredToolFunction{}, false, errors.New("index: tool function CasHash must not be empty")
	}

	// Axis 1: request_id idempotency / conflict.
	if existing, done, rerr := s.resolveToolFunctionRequestLocked(reqID, rec.CasHash); done {
		return existing, false, rerr
	}

	now := time.Now().UTC()

	// Axis 2: content idempotency by CasHash. An already-registered runnable record
	// is authoritative: return it unchanged (never resurrect/overwrite), and only
	// record the fresh request-id mapping so its later replays reconcile.
	if existing, ferr := s.findToolFunctionLocked(rec.CasHash); ferr == nil {
		if reqID == "" {
			return existing, false, nil
		}
		reqLen := len(s.idx.ToolFunctionRequestRecords)
		s.appendToolFunctionRequestRecordLocked(reqID, existing, now)
		if serr := s.save(); serr != nil {
			// Roll the in-memory append back so a failed persist is not masked by
			// the request ledger on a later retry (which would return success
			// without ever writing the mapping to disk) — but ONLY for a pre-rename
			// failure (disk unchanged). A post-rename durability failure already wrote
			// the mapping to disk, so keep memory consistent with disk and just surface
			// the durability error.
			if !errors.Is(serr, errIndexPersistedNotDurable) {
				s.idx.ToolFunctionRequestRecords = s.idx.ToolFunctionRequestRecords[:reqLen]
			} else {
				// Rename succeeded but the dir fsync did not: the append is on disk/in memory
				// but the rename may not be durable. Force a re-save before the next ack.
				s.toolFunctionDurabilityUncertain = true
			}
			return RegisteredToolFunction{}, false, serr
		}
		return existing, false, nil
	}

	// New registration.
	if rec.RegisteredAt.IsZero() {
		rec.RegisteredAt = now
	}
	if rec.LifecycleUpdatedAt.IsZero() {
		rec.LifecycleUpdatedAt = now
	}
	if rec.LifecyclePhase == "" {
		rec.LifecyclePhase = PhaseActive
	}
	if rec.ArtifactKind == "" {
		rec.ArtifactKind = KindToolFunction
	}
	recLen := len(s.idx.RegisteredToolFunctions)
	revLen := len(s.idx.ToolFunctionPresentationRevisions)
	reqLen := len(s.idx.ToolFunctionRequestRecords)
	s.idx.RegisteredToolFunctions = append(s.idx.RegisteredToolFunctions, rec)
	s.appendPresentationRevisionLocked(rev, now)
	if reqID != "" {
		s.appendToolFunctionRequestRecordLocked(reqID, rec, now)
	}

	if serr := s.save(); serr != nil {
		// A failed PRE-RENAME persist must leave no in-memory trace: otherwise the
		// request ledger would make a retry with the same request id return success
		// without the record ever reaching disk, losing it on restart. But a POST-RENAME
		// durability failure already wrote the record to disk; rolling memory back there
		// would diverge memory from disk and let a later save() delete the committed
		// record. So keep memory (it matches disk) and only surface the durability error.
		if !errors.Is(serr, errIndexPersistedNotDurable) {
			s.idx.RegisteredToolFunctions = s.idx.RegisteredToolFunctions[:recLen]
			s.idx.ToolFunctionPresentationRevisions = s.idx.ToolFunctionPresentationRevisions[:revLen]
			s.idx.ToolFunctionRequestRecords = s.idx.ToolFunctionRequestRecords[:reqLen]
		} else {
			// Rename succeeded but the dir fsync did not: the record is on disk/in memory but
			// the rename may not be durable. Force a re-save before the next ack (including an
			// idempotent replay).
			s.toolFunctionDurabilityUncertain = true
		}
		return RegisteredToolFunction{}, false, serr
	}
	return rec, true, nil
}

// resolveToolFunctionRequestLocked applies the request_id idempotency axis. done is
// true when the caller should return immediately: either an idempotent replay of the
// same content (existing record, nil error) or a conflict — the same request_id
// resolving to a different CasHash — returned fail-closed with no mutation. An empty
// reqID or an unseen reqID yields done=false so the content axis proceeds.
func (s *Store) resolveToolFunctionRequestLocked(reqID, casHash string) (RegisteredToolFunction, bool, error) {
	if reqID == "" {
		return RegisteredToolFunction{}, false, nil
	}
	for i := range s.idx.ToolFunctionRequestRecords {
		if s.idx.ToolFunctionRequestRecords[i].RequestID != reqID {
			continue
		}
		if s.idx.ToolFunctionRequestRecords[i].CasHash != casHash {
			return RegisteredToolFunction{}, true, fmt.Errorf("%w: request_id=%q", ErrToolFunctionRequestConflict, reqID)
		}
		existing, ferr := s.findToolFunctionLocked(casHash)
		if ferr != nil {
			return RegisteredToolFunction{}, true, fmt.Errorf(
				"index inconsistency: request %q maps to missing tool function %q: %w", reqID, casHash, ferr)
		}
		return existing, true, nil
	}
	return RegisteredToolFunction{}, false, nil
}

// appendToolFunctionRequestRecordLocked records a request_id -> runnable mapping.
//
//nolint:gocritic // hugeParam: RegisteredToolFunction by value is intentional — callers own their copy.
func (s *Store) appendToolFunctionRequestRecordLocked(reqID string, rec RegisteredToolFunction, now time.Time) {
	s.idx.ToolFunctionRequestRecords = append(s.idx.ToolFunctionRequestRecords, ToolFunctionRequestRecord{
		RequestID:              reqID,
		CasHash:                rec.CasHash,
		ToolFunctionDigest:     rec.ToolFunctionDigest,
		PresentationRevisionID: rec.PresentationRevisionID,
		CreatedAt:              now,
	})
}

// appendPresentationRevisionLocked appends a presentation revision, content-addressed
// by RevisionID and deduplicated so an identical presentation shared by several
// runnable records is stored once. A nil or id-less revision is a no-op.
func (s *Store) appendPresentationRevisionLocked(rev *ToolFunctionPresentationRevision, now time.Time) {
	if rev == nil || rev.RevisionID == "" {
		return
	}
	for i := range s.idx.ToolFunctionPresentationRevisions {
		if s.idx.ToolFunctionPresentationRevisions[i].RevisionID == rev.RevisionID {
			return
		}
	}
	r := *rev
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	s.idx.ToolFunctionPresentationRevisions = append(s.idx.ToolFunctionPresentationRevisions, r)
}

// GetToolFunctionByCasHash returns the runnable ToolFunction with the given CasHash.
// Returns ErrNotFound if none exists. The legacy GetTool/GetByCasHash path does not
// read this section, so a ToolFunction is never reinterpreted as a legacy Tool.
func (s *Store) GetToolFunctionByCasHash(casHash string) (RegisteredToolFunction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findToolFunctionLocked(casHash)
}

// GetToolFunctionPresentationRevision returns the presentation revision with the
// given RevisionID. Returns ErrNotFound if none exists.
func (s *Store) GetToolFunctionPresentationRevision(revisionID string) (ToolFunctionPresentationRevision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.idx.ToolFunctionPresentationRevisions {
		if s.idx.ToolFunctionPresentationRevisions[i].RevisionID == revisionID {
			return s.idx.ToolFunctionPresentationRevisions[i], nil
		}
	}
	return ToolFunctionPresentationRevision{}, fmt.Errorf("%w: revision_id=%q", ErrNotFound, revisionID)
}

// GetToolFunctionRequestRecord returns the request-id idempotency record with the
// given RequestID. Returns ErrNotFound if none exists.
func (s *Store) GetToolFunctionRequestRecord(requestID string) (ToolFunctionRequestRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.idx.ToolFunctionRequestRecords {
		if s.idx.ToolFunctionRequestRecords[i].RequestID == requestID {
			return s.idx.ToolFunctionRequestRecords[i], nil
		}
	}
	return ToolFunctionRequestRecord{}, fmt.Errorf("%w: request_id=%q", ErrNotFound, requestID)
}

func (s *Store) findToolFunctionLocked(casHash string) (RegisteredToolFunction, error) {
	for i := range s.idx.RegisteredToolFunctions {
		if s.idx.RegisteredToolFunctions[i].CasHash == casHash {
			return s.idx.RegisteredToolFunctions[i], nil
		}
	}
	return RegisteredToolFunction{}, fmt.Errorf("%w: cas_hash=%q", ErrNotFound, casHash)
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
	// A file stamped with a newer schema than this binary understands may
	// carry fields this code has never heard of. Silently unmarshaling it
	// would drop or misinterpret that data on the next save() — so reject it
	// outright rather than risk corrupting a newer writer's state. Older
	// files (SchemaVersion < schemaVersion) are fine: every bump so far has
	// only added optional fields/sections (see the schemaVersion doc
	// comment), so absent fields simply load as zero values/empty slices.
	if f.SchemaVersion > schemaVersion {
		return fmt.Errorf(
			"index: %s has schema_version %d, newer than this binary's supported version %d (refusing to load)",
			s.path, f.SchemaVersion, schemaVersion)
	}
	s.idx = &f
	return nil
}

// save persists the in-memory index to disk via a temp-file-then-rename
// sequence so a mid-write process kill (OOM-kill, pod eviction) can never
// leave s.path truncated or half-written: os.WriteFile truncates the
// existing file in place before writing, so a kill mid-write would
// permanently corrupt the only copy with no way to recover on restart. The
// temp file is created in the same directory as s.path so the final rename
// is same-filesystem and atomic, fsync'd before the rename so its content is
// durable on disk first, and cleaned up on any error path before the rename
// happens. The parent directory is fsync'd after the rename so the rename
// itself survives a crash.
func (s *Store) save() error {
	// Stamp the current schema version. Once this binary writes any section, the file
	// carries this version's shape (e.g. schema 5 ToolFunction sections), so it must be
	// labeled as such: a rolled-back older binary then refuses it via load()'s version
	// guard instead of silently dropping sections it cannot represent on its next save.
	s.idx.SchemaVersion = schemaVersion
	data, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal: %w", err)
	}

	dir := filepath.Dir(s.path)
	//nolint:gosec // dir is operator-configured and not from user input
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("index: create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Always attempt to remove the temp file; once the rename below
	// succeeds this is a no-op (nothing left at tmpPath to remove).
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("index: write temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("index: sync temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("index: close temp file %s: %w", tmpPath, err)
	}
	//nolint:gosec // 0o640 matches the previous os.WriteFile mode; path is operator-configured
	if err := os.Chmod(tmpPath, 0o640); err != nil {
		return fmt.Errorf("index: chmod temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("index: rename %s to %s: %w", tmpPath, s.path, err)
	}
	// Test-only seam: simulate a post-rename durability failure (nil in production). It runs
	// only after the rename has already swapped the file, exactly where a real parent-dir
	// fsync error would occur, so it exercises the errIndexPersistedNotDurable path.
	if failAfterRenameForTest != nil {
		if e := failAfterRenameForTest(); e != nil {
			return errors.Join(errIndexPersistedNotDurable, e)
		}
	}
	// PAST THIS POINT the replacement index is already atomically visible at s.path:
	// any further error is a post-rename DURABILITY error, not a failure to persist the
	// content. Such errors are tagged errIndexPersistedNotDurable so callers keep their
	// in-memory mutation (which now matches disk) instead of rolling it back — a rollback
	// here would diverge memory from disk and let a later save() overwrite the committed
	// file with the stale index, permanently losing the record.
	//
	// fsync the parent directory so the rename itself is durable. The temp file's contents
	// are fsync'd above, but the directory entry created by rename() is a separate metadata
	// write: without this a crash right after rename() could leave s.path still pointing at
	// the old inode (or absent) on restart, defeating the atomic-replace guarantee.
	if dirErr := syncDir(dir); dirErr != nil {
		return errors.Join(errIndexPersistedNotDurable, dirErr)
	}
	return nil
}

// syncDir fsyncs a directory so directory-entry changes (e.g. a rename) become durable.
// It is the shared parent-directory durability primitive used by save() (to make a rename
// durable) and by the Store constructors (to conservatively make any visible-but-un-fsync'd
// rename from before a crash durable at startup, before the Store serves requests).
func syncDir(dir string) error {
	if syncDirForTest != nil {
		return syncDirForTest(dir)
	}
	//nolint:gosec // dir is operator-configured and not from user input
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("index: open dir %s for fsync: %w", dir, err)
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return fmt.Errorf("index: sync dir %s: %w", dir, err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("index: close dir %s: %w", dir, err)
	}
	return nil
}
