package index

import (
	"errors"
	"testing"
)

// TestRegisterToolFunctionAtomic_NoResurrect proves re-registration of byte-identical
// content never resurrects a Retracted/Deleted runnable record to Active. W2 has no
// tool-function retract/delete RPC, so the non-Active existing state (with its provable
// w2-set-v1 originating receipt) is seeded directly (white-box) and persisted, to exercise the
// store guarantee that a content-hit returns the existing record's authoritative phase
// verbatim, never the freshly-fabricated Active.
func TestRegisterToolFunctionAtomic_NoResurrect(t *testing.T) {
	for _, phase := range []LifecyclePhase{PhaseRetracted, PhaseDeleted} {
		t.Run(string(phase), func(t *testing.T) {
			s, err := NewAt(t.TempDir())
			if err != nil {
				t.Fatalf("NewAt: %v", err)
			}
			origin := tfDurOp("req-origin")
			s.idx.RegisteredToolFunctions = append(s.idx.RegisteredToolFunctions, RegisteredToolFunction{
				CasHash:                 "casA",
				ToolFunctionDigest:      "tfd1",
				FunctionImageDigest:     "imgA",
				ArtifactKind:            KindToolFunction,
				RequestID:               origin.RequestID,
				CanonicalizationVersion: CanonicalizationW2SetV1,
				LifecyclePhase:          phase,
				IntegrityHealth:         HealthPartial,
			})
			s.idx.ToolFunctionRequestRecords = append(s.idx.ToolFunctionRequestRecords, ToolFunctionRequestRecord{
				RequestID:               origin.RequestID,
				CasHash:                 "casA",
				CanonicalizationVersion: origin.CanonicalizationVersion,
				RequestFingerprint:      origin.RequestFingerprint,
				RequestBasisJSON:        origin.RequestBasisJSON,
			})
			if serr := s.save(); serr != nil {
				t.Fatalf("seed save: %v", serr)
			}

			out, created, err := s.RegisterToolFunctionAtomic(tfDurOp("req-1"), RegisteredToolFunction{
				CasHash:             "casA",
				ToolFunctionDigest:  "tfd1",
				FunctionImageDigest: "imgA",
				ArtifactKind:        KindToolFunction,
				LifecyclePhase:      PhaseActive,
				IntegrityHealth:     HealthPartial,
			}, nil)
			if err != nil {
				t.Fatalf("re-register: %v", err)
			}
			if created {
				t.Fatal("re-registration of existing content must not create a new record")
			}
			if out.LifecyclePhase != phase {
				t.Fatalf("re-registration resurrected %s -> %s", phase, out.LifecyclePhase)
			}
		})
	}
}

// TestRegisterToolFunctionAtomic_LegacyRecordNotReused proves an UNKNOWN_LEGACY runnable record
// (no derivation version / receipt basis) is neither reused, relabeled nor resurrected by a new
// versioned request whose hash coincides: the registration is held with zero mutation (M11).
func TestRegisterToolFunctionAtomic_LegacyRecordNotReused(t *testing.T) {
	s, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	legacy := RegisteredToolFunction{
		CasHash:             "casA",
		ToolFunctionDigest:  "tfd1",
		FunctionImageDigest: "imgA",
		ArtifactKind:        KindToolFunction,
		RequestID:           "legacy-req",
		LifecyclePhase:      PhaseRetracted,
		IntegrityHealth:     HealthPartial,
	}
	s.idx.RegisteredToolFunctions = append(s.idx.RegisteredToolFunctions, legacy)
	s.idx.ToolFunctionRequestRecords = append(s.idx.ToolFunctionRequestRecords,
		ToolFunctionRequestRecord{RequestID: "legacy-req", CasHash: "casA", ToolFunctionDigest: "tfd1"})
	if err = s.save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	_, _, err = s.RegisterToolFunctionAtomic(tfDurOp("req-new"), tfDurRec("casA", "tfd1", "imgA"), nil)
	if !errors.Is(err, ErrToolFunctionIdentityAmbiguous) {
		t.Fatalf("want ErrToolFunctionIdentityAmbiguous, got %v", err)
	}
	if _, ok := s.findToolFunctionRequestLocked("req-new"); ok {
		t.Fatal("held registration must not record a receipt")
	}
	got, err := s.GetToolFunctionByCasHash("casA")
	if err != nil {
		t.Fatalf("legacy record lost: %v", err)
	}
	if got.CanonicalizationVersion != "" || got.LifecyclePhase != PhaseRetracted {
		t.Fatalf("legacy record was relabeled or resurrected: %+v", got)
	}
}
