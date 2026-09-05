package index

import "testing"

// TestRegisterToolFunctionAtomic_NoResurrect proves re-registration of byte-identical
// content never resurrects a Retracted/Deleted runnable record to Active. W2 has no
// tool-function retract/delete RPC, so the non-Active existing state is seeded directly
// (white-box) to exercise the store guarantee that a content-hit returns the existing
// record's authoritative phase verbatim, never the freshly-fabricated Active.
func TestRegisterToolFunctionAtomic_NoResurrect(t *testing.T) {
	for _, phase := range []LifecyclePhase{PhaseRetracted, PhaseDeleted} {
		t.Run(string(phase), func(t *testing.T) {
			s, err := NewAt(t.TempDir())
			if err != nil {
				t.Fatalf("NewAt: %v", err)
			}
			s.idx.RegisteredToolFunctions = append(s.idx.RegisteredToolFunctions, RegisteredToolFunction{
				CasHash:             "casA",
				ToolFunctionDigest:  "tfd1",
				FunctionImageDigest: "imgA",
				ArtifactKind:        KindToolFunction,
				LifecyclePhase:      phase,
				IntegrityHealth:     HealthPartial,
			})

			out, created, err := s.RegisterToolFunctionAtomic("req-1", RegisteredToolFunction{
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
