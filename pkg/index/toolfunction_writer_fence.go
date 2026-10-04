package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Canonicalizer versions of a RegisterToolFunction operation (issue #19 DC-R1-NV-C1).
const (
	// CanonicalizationLegacyOrderV0 is the replay-only compatibility branch: the original
	// order-preserving canonicalizer. It applies only to an existing receipt whose algorithm and
	// full basis are recorded; new writes never select it.
	CanonicalizationLegacyOrderV0 = "legacy-order-v0"
	// CanonicalizationW2SetV1 is the W2 FINAL canonicalizer for new registration writes:
	// command.arguments is ordered, the set-like fields are sorted and exact duplicates rejected.
	CanonicalizationW2SetV1 = "w2-set-v1"
)

const (
	toolFunctionWriterLockFileName  = "vault-index.toolfunction-writer.lock"
	toolFunctionWriterFenceFileName = "vault-index.toolfunction-writer-fence.json"
)

// ErrToolFunctionWriterFenced is returned by RegisterToolFunctionAtomic, and by every other
// persistent Store write once this Store has claimed a write epoch, when this Store is not the
// current ToolFunction writer: another writer claimed a newer write epoch, the durable fence was
// written by an unknown/newer profile, or the index was rewritten by an older (v1-unaware) binary
// after this writer committed. Every Store's save() also returns it when the index file changed
// on disk since that Store loaded it (a stale whole-index view, e.g. a Store that never claimed
// an epoch). The write is refused before the index file is touched so old and new writers never
// both acknowledge writes to the same authority store (DC-R1-NV-C1 cutover step 2 / N7).
var ErrToolFunctionWriterFenced = errors.New("index: tool function writer fenced")

// toolFunctionWriterFence is the durable activation marker and single serialized write epoch of
// the versioned ToolFunction writer. It lives in its own file next to the index so writers that
// never register ToolFunctions (e.g. NodePalette health updates) cannot regress it.
type toolFunctionWriterFence struct {
	Profile            string    `json:"profile"`
	Epoch              int64     `json:"epoch"`
	IndexSchemaVersion int       `json:"index_schema_version"`
	ActivatedAt        time.Time `json:"activated_at"`
	ClaimedAt          time.Time `json:"claimed_at"`
}

// lockToolFunctionWriter takes the cross-process exclusive ToolFunction writer lock, so the
// fence check/claim and the registration save of one writer are serialized against every other
// versioned writer on the same index directory.
func (s *Store) lockToolFunctionWriter() (unlock func(), err error) {
	path := filepath.Join(filepath.Dir(s.path), toolFunctionWriterLockFileName)
	//nolint:gosec // path is operator-configured index dir, not user input
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("index: open tool function writer lock %s: %w", path, err)
	}
	fd := int(f.Fd()) //nolint:gosec // G115: a file descriptor always fits in int
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("index: lock tool function writer %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// ensureToolFunctionWriterLocked verifies (or, on this Store's first registration, claims) the
// single ToolFunction write epoch. Must be called with s.mu and the writer lock held, before
// any persistent mutation.
func (s *Store) ensureToolFunctionWriterLocked(now time.Time) error {
	fence, err := s.readToolFunctionWriterFence()
	if err != nil {
		return err
	}
	if fence != nil {
		if fence.Profile != CanonicalizationW2SetV1 || fence.IndexSchemaVersion > schemaVersion {
			return fmt.Errorf("%w: on-disk fence profile %q / index schema %d is not supported by this binary",
				ErrToolFunctionWriterFenced, fence.Profile, fence.IndexSchemaVersion)
		}
	}
	if s.toolFunctionWriterEpoch != 0 {
		if fence == nil || fence.Epoch != s.toolFunctionWriterEpoch {
			onDisk := int64(0)
			if fence != nil {
				onDisk = fence.Epoch
			}
			return fmt.Errorf("%w: write epoch %d superseded (on-disk epoch %d)",
				ErrToolFunctionWriterFenced, s.toolFunctionWriterEpoch, onDisk)
		}
		if s.toolFunctionWriterCommitted {
			version, present, verr := s.readOnDiskSchemaVersion()
			if verr != nil {
				return verr
			}
			if present && version < schemaVersion {
				return fmt.Errorf("%w: index rewritten with schema_version %d after this writer committed schema %d",
					ErrToolFunctionWriterFenced, version, schemaVersion)
			}
		}
		return nil
	}

	// Claiming takes over from any previous writer: refresh the in-memory index from disk under
	// the writer lock first, so this writer never saves a stale view over registrations the
	// previous epoch committed.
	if err := s.load(); err != nil {
		return err
	}
	next := toolFunctionWriterFence{
		Profile:            CanonicalizationW2SetV1,
		Epoch:              1,
		IndexSchemaVersion: schemaVersion,
		ActivatedAt:        now,
		ClaimedAt:          now,
	}
	if fence != nil {
		next.Epoch = fence.Epoch + 1
		if !fence.ActivatedAt.IsZero() {
			next.ActivatedAt = fence.ActivatedAt
		}
	}
	if err := s.writeToolFunctionWriterFence(&next); err != nil {
		return err
	}
	s.toolFunctionWriterEpoch = next.Epoch
	return nil
}

func (s *Store) readToolFunctionWriterFence() (*toolFunctionWriterFence, error) {
	path := filepath.Join(filepath.Dir(s.path), toolFunctionWriterFenceFileName)
	data, err := os.ReadFile(path) //nolint:gosec // path is operator-configured index dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("index: read tool function writer fence %s: %w", path, err)
	}
	var f toolFunctionWriterFence
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("index: parse tool function writer fence %s: %w", path, err)
	}
	return &f, nil
}

// indexGenerationAbsent is the indexGeneration of a Store that loaded no index file.
const indexGenerationAbsent = "absent"

func indexGenerationOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checkIndexGenerationLocked reports whether the index file on disk differs from the bytes this
// Store last loaded or persisted. Must be called with s.mu and the writer lock held.
func (s *Store) checkIndexGenerationLocked() (moved bool, err error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.indexGeneration != indexGenerationAbsent, nil
		}
		return false, fmt.Errorf("index: read %s: %w", s.path, err)
	}
	return indexGenerationOf(data) != s.indexGeneration, nil
}

// beginToolFunctionWriteLocked verifies or claims the write epoch, then refreshes the in-memory
// index if another Store (e.g. one that never claims an epoch) saved since this one last loaded
// or persisted it. Nothing is mutated yet at this point, so reloading cannot drop a pending
// write; it keeps the registration's save from erasing the other Store's writes. Must be called
// with s.mu and the writer lock held.
func (s *Store) beginToolFunctionWriteLocked(now time.Time) error {
	if err := s.ensureToolFunctionWriterLocked(now); err != nil {
		return err
	}
	moved, err := s.checkIndexGenerationLocked()
	if err != nil || !moved {
		return err
	}
	return s.load()
}

// readOnDiskSchemaVersion returns the schema_version currently stamped on the index file.
func (s *Store) readOnDiskSchemaVersion() (version int, present bool, err error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("index: read %s: %w", s.path, err)
	}
	var h struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return 0, false, fmt.Errorf("index: parse %s: %w", s.path, err)
	}
	return h.SchemaVersion, true, nil
}

// writeToolFunctionWriterFence durably replaces the fence file (temp file, fsync, rename,
// parent-dir fsync), mirroring save().
func (s *Store) writeToolFunctionWriterFence(f *toolFunctionWriterFence) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal tool function writer fence: %w", err)
	}
	dir := filepath.Dir(s.path)
	path := filepath.Join(dir, toolFunctionWriterFenceFileName)
	//nolint:gosec // dir is operator-configured and not from user input
	tmp, err := os.CreateTemp(dir, toolFunctionWriterFenceFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("index: create temp fence file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("index: write temp fence file %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("index: sync temp fence file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("index: close temp fence file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("index: rename %s to %s: %w", tmpPath, path, err)
	}
	return syncDir(dir)
}
