package index_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// M18/N7: the fence covers every persistent write, not only registrations. After a takeover the
// superseded Store's non-ToolFunction writes (Append, lifecycle, build records) are refused
// before the file is touched, so its stale in-memory index cannot erase the record the newer
// epoch committed; the current writer keeps writing normally.
func TestToolFunctionCutover_FencedWriterRefusesNonToolFunctionWrites(t *testing.T) {
	dir := t.TempDir()
	s1, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s1: %v", err)
	}
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s2: %v", err)
	}
	if err = s1.Append(index.Entry{CasHash: "entry-0", LifecyclePhase: index.PhaseActive}); err != nil {
		t.Fatalf("s1 append before claiming an epoch: %v", err)
	}
	if _, _, err = s1.RegisterToolFunctionAtomic(tfOp("r1"), tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("s1 r1: %v", err)
	}
	if _, _, err = s2.RegisterToolFunctionAtomic(tfOp("r2"), tfRecord(casB, tfd1, imgB), nil); err != nil {
		t.Fatalf("s2 r2: %v", err)
	}

	for name, write := range map[string]func() error{
		"Append": func() error { return s1.Append(index.Entry{CasHash: "entry-1"}) },
		"SetLifecyclePhase": func() error {
			return s1.SetLifecyclePhase("entry-0", index.PhaseRetracted)
		},
		"AppendToolBuildRecord": func() error {
			return s1.AppendToolBuildRecord(index.ToolBuildRecord{BuildID: "build-1"})
		},
	} {
		if werr := write(); !errors.Is(werr, index.ErrToolFunctionWriterFenced) {
			t.Fatalf("superseded writer %s: want ErrToolFunctionWriterFenced, got %v", name, werr)
		}
	}

	if err = s2.Append(index.Entry{CasHash: "entry-2"}); err != nil {
		t.Fatalf("current writer append: %v", err)
	}
	s3, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, cas := range []string{casA, casB} {
		if _, err := s3.GetToolFunctionByCasHash(cas); err != nil {
			t.Fatalf("committed %s lost after fenced writes: %v", cas, err)
		}
	}
	if _, err := s3.GetByCasHash("entry-1"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("fenced Append persisted: %v", err)
	}
	if e, err := s3.GetByCasHash("entry-0"); err != nil || e.LifecyclePhase == index.PhaseRetracted {
		t.Fatalf("fenced lifecycle write persisted or entry lost: %+v err=%v", e, err)
	}
	if _, err := s3.GetToolBuildRecordByBuildID("build-1"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("fenced build record persisted: %v", err)
	}
	if _, err := s3.GetByCasHash("entry-2"); err != nil {
		t.Fatalf("current writer append lost: %v", err)
	}
}

// M18/N7 epoch-0 residual (GR 5b9e2ddf): a Store that never claimed an epoch and loaded before
// another Store registered casB must not save its stale view and erase casB. Its write is
// refused, it reloads, and a retry succeeds without losing casB.
func TestToolFunctionCutover_EpochZeroStaleStoreCannotEraseRegistration(t *testing.T) {
	dir := t.TempDir()
	s1, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s1: %v", err)
	}
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s2: %v", err)
	}
	if _, _, err = s2.RegisterToolFunctionAtomic(tfOp("r2"), tfRecord(casB, tfd1, imgB), nil); err != nil {
		t.Fatalf("s2 r2: %v", err)
	}

	if err = s1.Append(index.Entry{CasHash: "entry-1"}); !errors.Is(err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("stale epoch-0 Append: want ErrToolFunctionWriterFenced, got %v", err)
	}
	reopened, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err = reopened.GetToolFunctionByCasHash(casB); err != nil {
		t.Fatalf("casB erased by stale epoch-0 save: %v", err)
	}
	if _, err = reopened.GetByCasHash("entry-1"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("refused Append persisted: %v", err)
	}

	// The refusal reloaded s1: the retry sees casB and keeps it.
	if _, err = s1.GetToolFunctionByCasHash(casB); err != nil {
		t.Fatalf("refused Store did not reload: %v", err)
	}
	if err = s1.Append(index.Entry{CasHash: "entry-1"}); err != nil {
		t.Fatalf("retry after reload: %v", err)
	}
	reopened, err = index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen after retry: %v", err)
	}
	if _, err = reopened.GetToolFunctionByCasHash(casB); err != nil {
		t.Fatalf("casB lost after retry: %v", err)
	}
	if _, err = reopened.GetByCasHash("entry-1"); err != nil {
		t.Fatalf("retried Append lost: %v", err)
	}
}

// M18/N7 rollback-after-reload (GR 7e3ec653): when save() refuses a stale Store and reloads it,
// AppendToolImageRecord must not apply its pre-rename rollback to the reloaded index. Otherwise
// the other Store's committed image record is dropped from memory and the next write persists
// the loss.
func TestToolFunctionCutover_RefusedImageAppendKeepsOtherStoresRecord(t *testing.T) {
	dir := t.TempDir()
	s1, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s1: %v", err)
	}
	s2, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt s2: %v", err)
	}
	if err = s2.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:x"}); err != nil {
		t.Fatalf("s2 image x: %v", err)
	}

	if err = s1.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:y"}); !errors.Is(err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("stale image append: want ErrToolFunctionWriterFenced, got %v", err)
	}
	if _, err = s1.GetToolImageRecordByDigest("sha256:x"); err != nil {
		t.Fatalf("refused Store lost image x from memory: %v", err)
	}
	if _, err = s1.GetToolImageRecordByDigest("sha256:y"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("refused image y left in memory: %v", err)
	}
	if err = s1.Append(index.Entry{CasHash: "entry-1"}); err != nil {
		t.Fatalf("s1 next write: %v", err)
	}
	reopened, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err = reopened.GetToolImageRecordByDigest("sha256:x"); err != nil {
		t.Fatalf("image x erased by the refused Store's next write: %v", err)
	}
	if _, err = reopened.GetByCasHash("entry-1"); err != nil {
		t.Fatalf("s1 next write lost: %v", err)
	}
	if _, err = reopened.GetToolImageRecordByDigest("sha256:y"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("refused image y persisted: %v", err)
	}

	if err = s1.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:y"}); err != nil {
		t.Fatalf("retry image y: %v", err)
	}
	reopened, err = index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen after retry: %v", err)
	}
	for _, d := range []string{"sha256:x", "sha256:y"} {
		if _, err := reopened.GetToolImageRecordByDigest(d); err != nil {
			t.Fatalf("%s lost after retry: %v", d, err)
		}
	}
}

// A reloaded index can hold fewer image records than the stale view did (here the file was
// removed). The refused AppendToolImageRecord must neither panic on a reslice past the reloaded
// slice's capacity nor keep the stale records.
func TestToolFunctionCutover_RefusedImageAppendOnShorterReloadDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	s1, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	for _, d := range []string{"sha256:a", "sha256:b"} {
		if err = s1.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: d}); err != nil {
			t.Fatalf("append %s: %v", d, err)
		}
	}
	if err = os.Remove(filepath.Join(dir, "vault-index.json")); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	if err = s1.AppendToolImageRecord(index.ToolImageRecord{ImageDigest: "sha256:c"}); !errors.Is(err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("append over shorter reload: want ErrToolFunctionWriterFenced, got %v", err)
	}
	for _, d := range []string{"sha256:a", "sha256:b", "sha256:c"} {
		if _, err := s1.GetToolImageRecordByDigest(d); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("%s survived the reload: %v", d, err)
		}
	}
}

// The reverse direction: the current epoch writer must not erase a write that a fresh epoch-0
// Store committed after the writer last loaded. Its plain save is refused and reloads; its next
// registration refreshes from disk before mutating.
func TestToolFunctionCutover_EpochWriterDoesNotEraseEpochZeroWrite(t *testing.T) {
	dir := t.TempDir()
	w, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt writer: %v", err)
	}
	if _, _, err = w.RegisterToolFunctionAtomic(tfOp("r1"), tfRecord(casA, tfd1, imgA), nil); err != nil {
		t.Fatalf("writer r1: %v", err)
	}
	other, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt other: %v", err)
	}
	if err = other.Append(index.Entry{CasHash: "entry-x"}); err != nil {
		t.Fatalf("fresh epoch-0 append: %v", err)
	}

	if err = w.Append(index.Entry{CasHash: "entry-y"}); !errors.Is(err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("stale epoch writer Append: want ErrToolFunctionWriterFenced, got %v", err)
	}
	if err = w.Append(index.Entry{CasHash: "entry-y"}); err != nil {
		t.Fatalf("epoch writer retry after reload: %v", err)
	}
	if err = other.Append(index.Entry{CasHash: "entry-z"}); !errors.Is(err, index.ErrToolFunctionWriterFenced) {
		t.Fatalf("now-stale epoch-0 Append: want ErrToolFunctionWriterFenced, got %v", err)
	}
	if err = other.Append(index.Entry{CasHash: "entry-z"}); err != nil {
		t.Fatalf("epoch-0 retry after reload: %v", err)
	}
	if _, _, err = w.RegisterToolFunctionAtomic(tfOp("r2"), tfRecord(casB, tfd1, imgB), nil); err != nil {
		t.Fatalf("writer r2 after another Store's write: %v", err)
	}

	reopened, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, cas := range []string{"entry-x", "entry-y", "entry-z"} {
		if _, err := reopened.GetByCasHash(cas); err != nil {
			t.Fatalf("%s lost: %v", cas, err)
		}
	}
	for _, cas := range []string{casA, casB} {
		if _, err := reopened.GetToolFunctionByCasHash(cas); err != nil {
			t.Fatalf("%s lost: %v", cas, err)
		}
	}
}

// W2-OUTSIDE-DIGEST-REREG-01: a new request_id for an existing tool_function_digest under a
// different casHash must carry the same validation_policy and environment_hints. Each axis is
// diagnosed separately, absent and explicitly-empty are distinct, and a conflict leaves no
// record or receipt behind.
func TestToolFunctionCutover_DigestLevelEnvelope(t *testing.T) {
	const firstBasis = `{"spec":"s","validation_policy":{"p":1},"environment_hints":{"h":1}}`
	for _, tc := range []struct {
		name  string
		basis string
		axes  []string
	}{
		{"policy", `{"spec":"s","validation_policy":{"p":2},"environment_hints":{"h":1}}`, []string{"validation_policy"}},
		{"hints", `{"spec":"s","validation_policy":{"p":1},"environment_hints":{"h":2}}`, []string{"environment_hints"}},
		{"both", `{"spec":"s","validation_policy":{},"environment_hints":{"h":2}}`,
			[]string{"validation_policy", "environment_hints"}},
		{"absent vs empty", `{"spec":"s","validation_policy":{"p":1},"environment_hints":{}}`, []string{"environment_hints"}},
		{"absent", `{"spec":"s","validation_policy":{"p":1}}`, []string{"environment_hints"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := index.NewAt(dir)
			if err != nil {
				t.Fatalf("NewAt: %v", err)
			}
			first := tfOp("r1")
			first.RequestBasisJSON = firstBasis
			if _, _, err = s.RegisterToolFunctionAtomic(first, tfRecord(casA, tfd1, imgA), nil); err != nil {
				t.Fatalf("r1: %v", err)
			}
			other := tfOp("r2")
			other.RequestFingerprint = "fp-r2"
			other.RequestBasisJSON = tc.basis
			_, _, err = s.RegisterToolFunctionAtomic(other, tfRecord(casB, tfd1, imgB), nil)
			if !errors.Is(err, index.ErrToolFunctionEnvelopeConflict) {
				t.Fatalf("want ErrToolFunctionEnvelopeConflict, got %v", err)
			}
			for _, axis := range []string{"validation_policy", "environment_hints"} {
				want := false
				for _, a := range tc.axes {
					want = want || a == axis
				}
				if got := strings.Contains(err.Error(), axis); got != want {
					t.Fatalf("axis %s named=%v, want %v: %v", axis, got, want, err)
				}
			}
			if _, gerr := s.GetToolFunctionByCasHash(casB); !errors.Is(gerr, index.ErrNotFound) {
				t.Fatalf("conflict persisted a record: %v", gerr)
			}
			if n := countRequestRecords(t, dir); n != 1 {
				t.Fatalf("conflict mutated receipts: %d records", n)
			}
		})
	}

	t.Run("same envelope", func(t *testing.T) {
		s := newStore(t)
		first := tfOp("r1")
		first.RequestBasisJSON = firstBasis
		if _, _, err := s.RegisterToolFunctionAtomic(first, tfRecord(casA, tfd1, imgA), nil); err != nil {
			t.Fatalf("r1: %v", err)
		}
		other := tfOp("r2")
		other.RequestFingerprint = "fp-r2"
		other.RequestBasisJSON = `{"environment_hints":{"h":1},"image":"b","spec":"s","validation_policy":{"p":1}}`
		if _, created, err := s.RegisterToolFunctionAtomic(other, tfRecord(casB, tfd1, imgB), nil); err != nil || !created {
			t.Fatalf("same envelope, other image: created=%v err=%v", created, err)
		}
	})
}

// A new request_id for a tool_function_digest backed by an UNKNOWN_LEGACY record cannot prove the
// envelope, so it is held fail-closed (identity ambiguity, not a conflict) with zero mutation,
// even though its casHash differs from the legacy record's.
func TestToolFunctionCutover_DigestBackedByLegacyHeld(t *testing.T) {
	dir, s := seedLegacyIndex(t)
	_, _, err := s.RegisterToolFunctionAtomic(tfOp("new-req"), tfRecord("casNew", "tfdLegacy", "imgNew"), nil)
	if !errors.Is(err, index.ErrToolFunctionIdentityAmbiguous) {
		t.Fatalf("want ErrToolFunctionIdentityAmbiguous, got %v", err)
	}
	if _, gerr := s.GetToolFunctionByCasHash("casNew"); !errors.Is(gerr, index.ErrNotFound) {
		t.Fatalf("held registration persisted a record: %v", gerr)
	}
	if n := countRequestRecords(t, dir); n != 1 {
		t.Fatalf("held registration mutated receipts: %d records", n)
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
