package index_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// Store-level DC-R1-NV-C1 cutover tests (w2-set-v1 receipt, UNKNOWN_LEGACY, writer fence).

// legacyIndexJSON is a schema-6 index as written before versioned writes: one runnable record
// and its request receipt, neither carrying a canonicalizer version nor a request basis.
const legacyIndexJSON = `{
  "schema_version": 6,
  "entries": [],
  "registered_tool_functions": [
    {"cas_hash": "casLegacy", "tool_function_digest": "tfdLegacy", "function_image_digest": "imgLegacy",
     "artifact_kind": "tool_function", "request_id": "legacy-req", "lifecycle_phase": "Active",
     "integrity_health": "Partial", "registered_at": "2026-09-01T00:00:00Z"}
  ],
  "tool_function_request_records": [
    {"request_id": "legacy-req", "cas_hash": "casLegacy", "tool_function_digest": "tfdLegacy",
     "created_at": "2026-09-01T00:00:00Z"}
  ]
}`

func seedLegacyIndex(t *testing.T) (dir string, s *index.Store) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vault-index.json"), []byte(legacyIndexJSON), 0o600); err != nil {
		t.Fatalf("seed legacy index: %v", err)
	}
	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	return dir, s
}

func countRequestRecords(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "vault-index.json")) //nolint:gosec // G304: t.TempDir()
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var f struct {
		Records []json.RawMessage `json:"tool_function_request_records"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	return len(f.Records)
}

// M10: a legacy request_id replay without a full basis is UNKNOWN, with zero mutation.
func TestToolFunctionCutover_LegacyReplayUnknown(t *testing.T) {
	dir, s := seedLegacyIndex(t)
	rec := tfRecord("casLegacy", "tfdLegacy", "imgLegacy")
	_, _, err := s.RegisterToolFunctionAtomic(tfOp("legacy-req"), rec, nil)
	if !errors.Is(err, index.ErrToolFunctionRequestUnknownLegacy) {
		t.Fatalf("want ErrToolFunctionRequestUnknownLegacy, got %v", err)
	}
	if n := countRequestRecords(t, dir); n != 1 {
		t.Fatalf("legacy replay mutated receipts: %d records", n)
	}
}

// M1/M12/N5: an existing legacy record keeps its exact identity across the cutover — it is
// read back verbatim, never rehashed, relabeled or backfilled, even after the file is
// re-stamped by a versioned write.
func TestToolFunctionCutover_LegacyNotBackfilled(t *testing.T) {
	dir, s := seedLegacyIndex(t)
	if _, _, err := s.RegisterToolFunctionAtomic(tfOp("new-req"), tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("versioned registration: %v", err)
	}
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.GetToolFunctionByCasHash("casLegacy")
	if err != nil {
		t.Fatalf("legacy exact read: %v", err)
	}
	if got.ToolFunctionDigest != "tfdLegacy" || got.CanonicalizationVersion != "" {
		t.Fatalf("legacy record changed: %+v", got)
	}
	receipt, err := s2.GetToolFunctionRequestRecord("legacy-req")
	if err != nil {
		t.Fatalf("legacy receipt: %v", err)
	}
	if receipt.BasisKnown() || receipt.CanonicalizationVersion != "" {
		t.Fatalf("legacy receipt was backfilled: %+v", receipt)
	}
	fresh, err := s2.GetToolFunctionRequestRecord("new-req")
	if err != nil {
		t.Fatalf("new receipt: %v", err)
	}
	if fresh.CanonicalizationVersion != index.CanonicalizationW2SetV1 || fresh.RequestFingerprint != "fp" ||
		fresh.RequestBasisJSON == "" {
		t.Fatalf("new receipt lacks version/basis: %+v", fresh)
	}
	created, err := s2.GetToolFunctionByCasHash(casA)
	if err != nil || created.CanonicalizationVersion != index.CanonicalizationW2SetV1 {
		t.Fatalf("new record derivation version: %+v err=%v", created, err)
	}
}

// M7/M8: the same request_id with a different request basis or canonicalizer version is a
// conflict with zero mutation.
func TestToolFunctionCutover_ReplayComparesVersionAndBasis(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*index.ToolFunctionOperation)
	}{
		{"basis", func(op *index.ToolFunctionOperation) { op.RequestFingerprint = "fp-other" }},
		{"version", func(op *index.ToolFunctionOperation) {
			op.CanonicalizationVersion = index.CanonicalizationLegacyOrderV0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := index.NewAt(dir)
			if err != nil {
				t.Fatalf("NewAt: %v", err)
			}
			if _, _, ferr := s.RegisterToolFunctionAtomic(tfOp(reqID1), tfRecord(casA, tfd1, imgA), nil); ferr != nil {
				t.Fatalf("first: %v", ferr)
			}
			op := tfOp(reqID1)
			tc.mut(&op)
			_, _, err = s.RegisterToolFunctionAtomic(op, tfRecord(casA, tfd1, imgA), nil)
			if !errors.Is(err, index.ErrToolFunctionRequestConflict) {
				t.Fatalf("want ErrToolFunctionRequestConflict, got %v", err)
			}
			if n := countRequestRecords(t, dir); n != 1 {
				t.Fatalf("conflict mutated receipts: %d records", n)
			}
		})
	}
}

// N6: a new request_id reusing an existing v1 identity must prove the same full basis.
func TestToolFunctionCutover_ReuseRequiresSameBasis(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.RegisterToolFunctionAtomic(tfOp("reqA"), tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("reqA: %v", err)
	}
	other := tfOp("reqB")
	other.RequestFingerprint = "fp-different-envelope"
	if _, _, err := s.RegisterToolFunctionAtomic(other, tfRecord(casA, tfd1, imgA), nil); !errors.Is(
		err, index.ErrToolFunctionIdentityAmbiguous) {
		t.Fatalf("want ErrToolFunctionIdentityAmbiguous, got %v", err)
	}
	if _, err := s.GetToolFunctionRequestRecord("reqB"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("held reuse must not record a receipt: %v", err)
	}
}

// An incomplete operation (no version/basis) can never mint an UNKNOWN_LEGACY receipt.
func TestToolFunctionCutover_IncompleteOperationRejected(t *testing.T) {
	s := newStore(t)
	op := tfOp(reqID1)
	op.CanonicalizationVersion = ""
	if _, _, err := s.RegisterToolFunctionAtomic(op, tfRecord(casA, tfd1, imgA), nil); err == nil {
		t.Fatal("expected rejection of an operation without a canonicalizer version")
	}
	if _, err := s.GetToolFunctionByCasHash(casA); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("rejected operation persisted a record: %v", err)
	}
}

// M17: concurrent replays of the same request_id produce one record and one receipt.
func TestToolFunctionCutover_ConcurrentReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.RegisterToolFunctionAtomic(tfOp(reqID1), tfRecord(casA, tfd1, imgA), nil)
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d: %v", i, e)
		}
	}
	if got := countRequestRecords(t, dir); got != 1 {
		t.Fatalf("want exactly one receipt, got %d", got)
	}
}

// M18: two versioned writers on the same index directory serialize on one write epoch. The
// later claimant takes over (after reloading the earlier writer's commits) and the superseded
// writer is refused for every write, including a replay, with zero mutation.
func TestToolFunctionCutover_WriterEpochFence(t *testing.T) {
	dir := t.TempDir()
	s1, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s1: %v", err)
	}
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s2: %v", err)
	}
	if _, _, err = s1.RegisterToolFunctionAtomic(tfOp("r1"), tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("s1 r1: %v", err)
	}
	if _, _, err = s2.RegisterToolFunctionAtomic(tfOp("r2"), tfRecord(casB, tfd1, imgB), nil); err != nil {
		t.Fatalf("s2 r2: %v", err)
	}
	if _, err = s2.GetToolFunctionByCasHash(casA); err != nil {
		t.Fatalf("taking-over writer lost the previous epoch's commit: %v", err)
	}
	if _, _, err = s1.RegisterToolFunctionAtomic(tfOp("r3"), tfRecord("casC", tfd1, "imgC"), nil); !errors.Is(
		err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("superseded writer: want ErrToolFunctionWriterFenced, got %v", err)
	}
	if _, _, err = s1.RegisterToolFunctionAtomic(tfOp("r1"), tfRecord(casA, tfd1, imgA), nil); !errors.Is(
		err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("superseded writer replay: want ErrToolFunctionWriterFenced, got %v", err)
	}
	s3, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s3.GetToolFunctionByCasHash("casC"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("fenced write persisted: %v", err)
	}
	for _, cas := range []string{casA, casB} {
		if _, err := s3.GetToolFunctionByCasHash(cas); err != nil {
			t.Fatalf("committed %s lost: %v", cas, err)
		}
	}
}

// M18 rollback/downgrade: once this writer committed schema 7, an index rewritten by an older
// (v1-unaware) binary is detected and the writer stops instead of acknowledging writes.
func TestToolFunctionCutover_DowngradeRewriteFenced(t *testing.T) {
	dir := t.TempDir()
	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if _, _, err := s.RegisterToolFunctionAtomic(tfOp("r1"), tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("r1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vault-index.json"),
		[]byte(`{"schema_version":6,"entries":[]}`), 0o600); err != nil {
		t.Fatalf("simulate old-binary rewrite: %v", err)
	}
	if _, _, err := s.RegisterToolFunctionAtomic(tfOp("r2"), tfRecord(casB, tfd1, imgB), nil); !errors.Is(
		err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("want ErrToolFunctionWriterFenced after downgrade rewrite, got %v", err)
	}
}

// N7: a fence written by an unknown (newer) profile is never overridden by this binary.
func TestToolFunctionCutover_UnknownFenceProfileRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vault-index.toolfunction-writer-fence.json"),
		[]byte(`{"profile":"w9-future","epoch":4,"index_schema_version":9}`), 0o600); err != nil {
		t.Fatalf("seed fence: %v", err)
	}
	s, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if _, _, err := s.RegisterToolFunctionAtomic(tfOp("r1"), tfRecord(casA, tfd1, imgA), nil); !errors.Is(
		err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("want ErrToolFunctionWriterFenced for unknown fence profile, got %v", err)
	}
	if _, err := s.GetToolFunctionByCasHash(casA); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("fenced write persisted: %v", err)
	}
}
