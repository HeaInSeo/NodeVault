package catalog

import (
	"math"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func twoInputSpec(first, second string) *nfv1.ToolFunctionSpec {
	return &nfv1.ToolFunctionSpec{
		Command: &nfv1.CommandContract{Executable: "bwa"},
		Inputs: []*nfv1.FunctionPortSpec{
			{Name: first, DataFormat: "fastq"},
			{Name: second, DataFormat: "fastq"},
		},
	}
}

func specDigest(t *testing.T, version string, spec *nfv1.ToolFunctionSpec) string {
	t.Helper()
	canon, err := canonicalToolFunctionSpecFor(version, spec)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", version, err)
	}
	d, err := canonicalSHA256(map[string]any{"base_tool_spec_digest": strings.Repeat("ab", 32), "spec": canon})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d
}

// TestLegacyOrderV0_PreservesAuthoredOrder pins the replay-only legacy-order-v0 branch: it is
// the original canonicalizer, so reordering inputs still changes its digest, while w2-set-v1
// sorts the same permutation to one digest (the behavior previously asserted by
// TestRegisterToolFunction_RepeatedFieldOrdering moved here, not deleted).
func TestLegacyOrderV0_PreservesAuthoredOrder(t *testing.T) {
	ab, ba := twoInputSpec("a", "b"), twoInputSpec("b", "a")
	if specDigest(t, index.CanonicalizationLegacyOrderV0, ab) == specDigest(t, index.CanonicalizationLegacyOrderV0, ba) {
		t.Fatal("legacy-order-v0 must keep authored order identity-bearing")
	}
	if specDigest(t, index.CanonicalizationW2SetV1, ab) != specDigest(t, index.CanonicalizationW2SetV1, ba) {
		t.Fatal("w2-set-v1 must sort set-like inputs")
	}
}

// TestLegacyOrderV0_DigestUnchanged proves the legacy branch reproduces the pre-cutover digest
// bytes exactly: for a spec with a single entry per repeated field (where order cannot matter)
// both canonicalizers agree, so no existing identity is reinterpreted by the version split.
func TestLegacyOrderV0_DigestUnchanged(t *testing.T) {
	spec := twoInputSpec("a", "b")
	spec.Inputs = spec.Inputs[:1]
	if specDigest(t, index.CanonicalizationLegacyOrderV0, spec) != specDigest(t, index.CanonicalizationW2SetV1, spec) {
		t.Fatal("single-entry spec must canonicalize identically under both versions")
	}
}

// TestCanonicalSHA256_EncodeErrorFailsClosed is M14: a canonical encoding failure returns an
// error and never a sentinel identity.
func TestCanonicalSHA256_EncodeErrorFailsClosed(t *testing.T) {
	d, err := canonicalSHA256(map[string]any{"bad": math.NaN()})
	if err == nil || d != "" {
		t.Fatalf("encode failure must return an error and no digest, got %q, %v", d, err)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("encode failure code = %v, want Internal", status.Code(err))
	}
	m := map[string]any{"set": []any{math.Inf(1)}}
	if serr := sortCanonicalSetIn(m, "set", "set"); status.Code(serr) != codes.Internal {
		t.Fatalf("set element encode failure: want Internal, got %v", serr)
	}
}
