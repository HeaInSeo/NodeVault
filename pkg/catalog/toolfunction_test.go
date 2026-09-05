package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func newTFService(t *testing.T) (*catalog.ToolRegistryService, *index.Store) {
	t.Helper()
	cat := catalog.NewCatalogAt(t.TempDir())
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	// RegisterToolFunction requires base_tool_spec_digest to resolve to an existing
	// ResolvedToolSpec, so seed the base used by validTFReq.
	seedResolvedToolSpec(t, store, baseDigest)
	return catalog.NewToolRegistryService(cat, store), store
}

// seedResolvedToolSpec registers an authoritative ResolvedToolSpec so a ToolFunction whose
// base_tool_spec_digest equals digest passes the base-existence check.
func seedResolvedToolSpec(t *testing.T, store *index.Store, digest string) {
	t.Helper()
	if err := store.AppendResolvedToolSpec(index.ResolvedToolSpec{ToolSpecDigest: digest}); err != nil {
		t.Fatalf("seed ResolvedToolSpec %q: %v", digest, err)
	}
}

func imgDigest(c byte) string {
	return "sha256:" + strings.Repeat(string(rune(c)), 64)
}

// baseDigest is a valid 64-char lowercase hex sha256 (resolve.ToolSpecDigest form).
var baseDigest = strings.Repeat("ab", 32)

const (
	fmtFastq    = "fastq"
	portAligned = "aligned"
)

// validTFReq returns a structurally valid RegisterToolFunctionRequest.
func validTFReq() *nfv1.RegisterToolFunctionRequest {
	return &nfv1.RegisterToolFunctionRequest{
		RequestId:          "req-1",
		BaseToolSpecDigest: baseDigest,
		ImageDigest:        imgDigest('a'),
		Spec: &nfv1.ToolFunctionSpec{
			Command: &nfv1.CommandContract{Executable: "bwa", Arguments: []string{"mem", "-t"}},
			Inputs: []*nfv1.FunctionPortSpec{
				{Name: "reads", DataFormat: fmtFastq, Cardinality: nfv1.Cardinality_CARDINALITY_SINGLE, Required: true},
			},
			Outputs: []*nfv1.FunctionPortSpec{
				{Name: portAligned, DataFormat: "bam"},
			},
			Parameters: []*nfv1.ParameterSpec{
				{Name: "threads", Type: nfv1.ParameterType_PARAMETER_TYPE_INTEGER},
			},
		},
		Presentation: &nfv1.ToolFunctionPresentation{
			Label: "BWA",
			OutputPortPresentations: []*nfv1.OutputPortPresentation{
				{PortName: portAligned, DownstreamCompatibilityNote: "sorted"},
			},
		},
	}
}

func mustRegisterTF(t *testing.T, svc *catalog.ToolRegistryService, req *nfv1.RegisterToolFunctionRequest) *nfv1.RegisterToolFunctionResponse {
	t.Helper()
	resp, err := svc.RegisterToolFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("RegisterToolFunction: %v", err)
	}
	return resp
}

func TestRegisterToolFunction_Success(t *testing.T) {
	svc, store := newTFService(t)
	resp := mustRegisterTF(t, svc, validTFReq())

	if resp.GetCasHash() == "" || resp.GetToolFunctionDigest() == "" {
		t.Fatalf("empty identity in response: %+v", resp)
	}
	if resp.GetPresentationRevisionId() == "" {
		t.Fatal("expected a presentation revision id")
	}
	rec, err := store.GetToolFunctionByCasHash(resp.GetCasHash())
	if err != nil {
		t.Fatalf("GetToolFunctionByCasHash: %v", err)
	}
	if rec.LifecyclePhase != index.PhaseActive {
		t.Fatalf("new registration phase = %q, want Active", rec.LifecyclePhase)
	}
	if rec.ArtifactKind != index.KindToolFunction {
		t.Fatalf("artifact kind = %q, want tool_function", rec.ArtifactKind)
	}
	if rec.FunctionImageDigest != validTFReq().GetImageDigest() {
		t.Fatalf("function image digest not persisted: %q", rec.FunctionImageDigest)
	}
	// The presentation revision is durably readable back.
	if _, err := store.GetToolFunctionPresentationRevision(resp.GetPresentationRevisionId()); err != nil {
		t.Fatalf("presentation revision not readable back: %v", err)
	}
}

// TestRegisterToolFunction_DigestMembership proves exactly which inputs belong to each
// digest: image_digest ∈ cas_hash only; spec & base_tool_spec_digest ∈ both; and
// presentation/policy are digest-out (do not change cas_hash or tool_function_digest).
func TestRegisterToolFunction_DigestMembership(t *testing.T) {
	svc, _ := newTFService(t)
	base := mustRegisterTF(t, svc, validTFReq())

	// Different function image → different cas_hash, SAME tool_function_digest.
	svc2, _ := newTFService(t)
	r2 := validTFReq()
	r2.ImageDigest = imgDigest('c')
	resp2 := mustRegisterTF(t, svc2, r2)
	if resp2.GetToolFunctionDigest() != base.GetToolFunctionDigest() {
		t.Fatal("image change must not alter tool_function_digest")
	}
	if resp2.GetCasHash() == base.GetCasHash() {
		t.Fatal("image change must alter cas_hash")
	}

	// Different spec → different tool_function_digest AND cas_hash.
	svc3, _ := newTFService(t)
	r3 := validTFReq()
	r3.Spec.Command.Executable = "minimap2"
	resp3 := mustRegisterTF(t, svc3, r3)
	if resp3.GetToolFunctionDigest() == base.GetToolFunctionDigest() {
		t.Fatal("spec change must alter tool_function_digest")
	}
	if resp3.GetCasHash() == base.GetCasHash() {
		t.Fatal("spec change must alter cas_hash")
	}

	// Different base_tool_spec_digest → different tool_function_digest AND cas_hash.
	svc4, store4 := newTFService(t)
	r4 := validTFReq()
	r4.BaseToolSpecDigest = strings.Repeat("c", 63) + "d"
	seedResolvedToolSpec(t, store4, r4.BaseToolSpecDigest) // base must resolve
	resp4 := mustRegisterTF(t, svc4, r4)
	if resp4.GetToolFunctionDigest() == base.GetToolFunctionDigest() {
		t.Fatal("base_tool_spec_digest change must alter tool_function_digest")
	}

	// Different presentation only → SAME cas_hash and SAME tool_function_digest
	// (presentation is digest-out). Registered on a fresh service so it is a new
	// registration rather than an idempotent hit.
	svc5, _ := newTFService(t)
	r5 := validTFReq()
	r5.Presentation.Label = "A totally different label"
	resp5 := mustRegisterTF(t, svc5, r5)
	if resp5.GetCasHash() != base.GetCasHash() {
		t.Fatal("presentation change must not alter cas_hash (digest-out)")
	}
	if resp5.GetToolFunctionDigest() != base.GetToolFunctionDigest() {
		t.Fatal("presentation change must not alter tool_function_digest (digest-out)")
	}
	if resp5.GetPresentationRevisionId() == base.GetPresentationRevisionId() {
		t.Fatal("different presentation must produce a different presentation revision id")
	}
}

// TestRegisterToolFunction_DigestInputNormalization proves the two identity-bearing
// digest inputs are canonicalized (trim + lowercase), so case/whitespace-variant
// spellings of the same digest converge to one identity (N2).
func TestRegisterToolFunction_DigestInputNormalization(t *testing.T) {
	svc1, _ := newTFService(t)
	base := mustRegisterTF(t, svc1, validTFReq())

	svc2, _ := newTFService(t)
	variant := validTFReq()
	variant.ImageDigest = "  " + strings.ToUpper(validTFReq().GetImageDigest()) + "  "
	variant.BaseToolSpecDigest = strings.ToUpper(baseDigest)
	resp := mustRegisterTF(t, svc2, variant)

	if resp.GetCasHash() != base.GetCasHash() {
		t.Fatal("case/whitespace-variant digest inputs must converge to the same cas_hash")
	}
	if resp.GetToolFunctionDigest() != base.GetToolFunctionDigest() {
		t.Fatal("case/whitespace-variant digest inputs must converge to the same tool_function_digest")
	}
}

// TestRegisterToolFunction_RepeatedFieldOrdering proves repeated-field order is
// identity-bearing: reordering inputs changes tool_function_digest.
func TestRegisterToolFunction_RepeatedFieldOrdering(t *testing.T) {
	svc1, _ := newTFService(t)
	r1 := validTFReq()
	r1.Spec.Inputs = []*nfv1.FunctionPortSpec{
		{Name: "a", DataFormat: fmtFastq},
		{Name: "b", DataFormat: fmtFastq},
	}
	resp1 := mustRegisterTF(t, svc1, r1)

	svc2, _ := newTFService(t)
	r2 := validTFReq()
	r2.Spec.Inputs = []*nfv1.FunctionPortSpec{
		{Name: "b", DataFormat: fmtFastq},
		{Name: "a", DataFormat: fmtFastq},
	}
	resp2 := mustRegisterTF(t, svc2, r2)

	if resp1.GetToolFunctionDigest() == resp2.GetToolFunctionDigest() {
		t.Fatal("reordering inputs must change tool_function_digest (order is identity-bearing)")
	}
}

func TestRegisterToolFunction_CardinalityMultipleRejected(t *testing.T) {
	svc, store := newTFService(t)
	req := validTFReq()
	req.Spec.Inputs[0].Cardinality = nfv1.Cardinality_CARDINALITY_MULTIPLE

	_, err := svc.RegisterToolFunction(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for MULTIPLE, got %v", err)
	}
	// Zero persistent mutation: the rejected registration recorded nothing (its
	// request_id is absent from the store).
	if _, gerr := store.GetToolFunctionRequestRecord(req.GetRequestId()); !errors.Is(gerr, index.ErrNotFound) {
		t.Fatalf("rejected registration must not persist a request record; got %v", gerr)
	}
	svcOut, _ := newTFService(t)
	reqOut := validTFReq()
	reqOut.Spec.Outputs[0].Cardinality = nfv1.Cardinality_CARDINALITY_MULTIPLE
	if _, err := svcOut.RegisterToolFunction(context.Background(), reqOut); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for MULTIPLE on output, got %v", err)
	}
}

// TestRegisterToolFunction_UnspecifiedNotNormalizedToSingle proves an UNSPECIFIED
// cardinality is omitted (not rewritten to SINGLE): a spec with UNSPECIFIED yields a
// different tool_function_digest than the same spec with explicit SINGLE.
func TestRegisterToolFunction_UnspecifiedNotNormalizedToSingle(t *testing.T) {
	svcU, _ := newTFService(t)
	rU := validTFReq()
	rU.Spec.Inputs[0].Cardinality = nfv1.Cardinality_CARDINALITY_UNSPECIFIED
	respU := mustRegisterTF(t, svcU, rU)

	svcS, _ := newTFService(t)
	rS := validTFReq()
	rS.Spec.Inputs[0].Cardinality = nfv1.Cardinality_CARDINALITY_SINGLE
	respS := mustRegisterTF(t, svcS, rS)

	if respU.GetToolFunctionDigest() == respS.GetToolFunctionDigest() {
		t.Fatal("UNSPECIFIED must not be normalized to SINGLE (digests must differ)")
	}
}

func TestRegisterToolFunction_DuplicateNamesRejected(t *testing.T) {
	cases := []struct {
		name  string
		mutfn func(*nfv1.RegisterToolFunctionRequest)
	}{
		{"dup input", func(r *nfv1.RegisterToolFunctionRequest) {
			r.Spec.Inputs = append(r.Spec.Inputs, &nfv1.FunctionPortSpec{Name: "reads", DataFormat: fmtFastq})
		}},
		{"dup output", func(r *nfv1.RegisterToolFunctionRequest) {
			r.Spec.Outputs = append(r.Spec.Outputs, &nfv1.FunctionPortSpec{Name: portAligned, DataFormat: "bam"})
		}},
		{"dup param", func(r *nfv1.RegisterToolFunctionRequest) {
			r.Spec.Parameters = append(r.Spec.Parameters, &nfv1.ParameterSpec{Name: "threads"})
		}},
		{"empty input name", func(r *nfv1.RegisterToolFunctionRequest) {
			r.Spec.Inputs[0].Name = "  "
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTFService(t)
			req := validTFReq()
			tc.mutfn(req)
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestRegisterToolFunction_PresentationReferentialIntegrity(t *testing.T) {
	// port_name not matching a declared output.
	svc1, _ := newTFService(t)
	r1 := validTFReq()
	r1.Presentation.OutputPortPresentations[0].PortName = "nonexistent"
	if _, err := svc1.RegisterToolFunction(context.Background(), r1); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for unknown presentation port, got %v", err)
	}
	// duplicate port_name presentation entries.
	svc2, _ := newTFService(t)
	r2 := validTFReq()
	r2.Presentation.OutputPortPresentations = append(r2.Presentation.OutputPortPresentations,
		&nfv1.OutputPortPresentation{PortName: portAligned, DownstreamCompatibilityNote: "dup"})
	if _, err := svc2.RegisterToolFunction(context.Background(), r2); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for duplicate presentation port, got %v", err)
	}
}

func TestRegisterToolFunction_ValidationPolicyConflict(t *testing.T) {
	svc, _ := newTFService(t)
	req := validTFReq()
	req.ValidationPolicy = &nfv1.ToolFunctionValidationPolicy{
		ExpectedResults: []*nfv1.ExpectedResult{{OutputPortName: "ghost", ExpectedValueOrRule: "x>0"}},
	}
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for policy referencing unknown output, got %v", err)
	}
}

// TestRegisterToolFunction_TypedKindReadAuthority proves the legacy tool/GetTool read
// path cannot reinterpret a ToolFunction: GetTool returns NotFound for its cas_hash,
// while the typed read returns it with the tool_function kind.
func TestRegisterToolFunction_TypedKindReadAuthority(t *testing.T) {
	svc, store := newTFService(t)
	resp := mustRegisterTF(t, svc, validTFReq())

	if _, err := svc.GetTool(context.Background(), &nfv1.GetToolRequest{CasHash: resp.GetCasHash()}); status.Code(err) != codes.NotFound {
		t.Fatalf("legacy GetTool must not resolve a ToolFunction; got %v", err)
	}
	rec, err := store.GetToolFunctionByCasHash(resp.GetCasHash())
	if err != nil || rec.ArtifactKind != index.KindToolFunction {
		t.Fatalf("typed read failed: rec=%+v err=%v", rec, err)
	}
}

func TestRegisterToolFunction_RequestIDIdempotent(t *testing.T) {
	svc, store := newTFService(t)
	first := mustRegisterTF(t, svc, validTFReq())
	second := mustRegisterTF(t, svc, validTFReq()) // same request_id, same content

	if first.GetCasHash() != second.GetCasHash() ||
		first.GetToolFunctionDigest() != second.GetToolFunctionDigest() ||
		first.GetPresentationRevisionId() != second.GetPresentationRevisionId() {
		t.Fatalf("idempotent replay diverged: %+v vs %+v", first, second)
	}
	// Exactly one record persisted.
	if _, err := store.GetToolFunctionRequestRecord("req-1"); err != nil {
		t.Fatalf("request record missing: %v", err)
	}
}

func TestRegisterToolFunction_RequestIDConflict(t *testing.T) {
	svc, _ := newTFService(t)
	mustRegisterTF(t, svc, validTFReq())

	conflict := validTFReq()              // same request_id...
	conflict.ImageDigest = imgDigest('e') // ...different content → different cas_hash
	_, err := svc.RegisterToolFunction(context.Background(), conflict)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists for request_id conflict, got %v", err)
	}
}

func TestRegisterToolFunction_InvalidRequests(t *testing.T) {
	svc, _ := newTFService(t)
	ctx := context.Background()

	if _, err := svc.RegisterToolFunction(ctx, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil request: got %v", err)
	}
	noBase := validTFReq()
	noBase.BaseToolSpecDigest = "   "
	if _, err := svc.RegisterToolFunction(ctx, noBase); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing base_tool_spec_digest: got %v", err)
	}
	badBase := validTFReq()
	badBase.BaseToolSpecDigest = "not-a-digest"
	if _, err := svc.RegisterToolFunction(ctx, badBase); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed base_tool_spec_digest: got %v", err)
	}
	badCard := validTFReq()
	badCard.Spec.Inputs[0].Cardinality = nfv1.Cardinality(99)
	if _, err := svc.RegisterToolFunction(ctx, badCard); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown cardinality enum: got %v", err)
	}
	badImg := validTFReq()
	badImg.ImageDigest = "not-a-digest"
	if _, err := svc.RegisterToolFunction(ctx, badImg); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unpinned image_digest: got %v", err)
	}
	noSpec := validTFReq()
	noSpec.Spec = nil
	if _, err := svc.RegisterToolFunction(ctx, noSpec); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing spec: got %v", err)
	}
}

// TestRegisterToolFunction_Concurrent proves atomic idempotent registration under
// concurrency: N identical requests all succeed and converge to one stored record.
func TestRegisterToolFunction_Concurrent(t *testing.T) {
	svc, store := newTFService(t)
	const n = 12
	var wg sync.WaitGroup
	hashes := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := svc.RegisterToolFunction(context.Background(), validTFReq())
			errs[i] = err
			if resp != nil {
				hashes[i] = resp.GetCasHash()
			}
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if hashes[i] != hashes[0] {
			t.Fatalf("goroutine %d cas_hash diverged: %q vs %q", i, hashes[i], hashes[0])
		}
	}
	if _, err := store.GetToolFunctionByCasHash(hashes[0]); err != nil {
		t.Fatalf("record missing after concurrent registration: %v", err)
	}
}

// TestRegisterToolFunction_ContentIdempotentAcrossRequestIDs proves two different
// request_ids carrying identical content converge to the same runnable record.
func TestRegisterToolFunction_ContentIdempotentAcrossRequestIDs(t *testing.T) {
	svc, store := newTFService(t)
	r1 := validTFReq()
	r1.RequestId = "reqA"
	resp1 := mustRegisterTF(t, svc, r1)

	r2 := validTFReq()
	r2.RequestId = "reqB"
	resp2 := mustRegisterTF(t, svc, r2)

	if resp1.GetCasHash() != resp2.GetCasHash() {
		t.Fatal("identical content under different request_ids must converge to one cas_hash")
	}
	recs := 0
	// Both request records must map to the same cas_hash.
	for _, id := range []string{"reqA", "reqB"} {
		rec, err := store.GetToolFunctionRequestRecord(id)
		if err != nil {
			t.Fatalf("request record %q: %v", id, err)
		}
		if rec.CasHash != resp1.GetCasHash() {
			t.Fatalf("request %q maps to %q, want %q", id, rec.CasHash, resp1.GetCasHash())
		}
		recs++
	}
	if recs != 2 {
		t.Fatalf("expected 2 request records, got %d", recs)
	}
}

// TestRegisterToolFunction_RequestIDRequired covers the P2 idempotency-key gate: an empty
// request_id is rejected before any mutation, since without it a lost-response retry whose
// spec/image changed would be accepted as a second runnable record.
func TestRegisterToolFunction_RequestIDRequired(t *testing.T) {
	svc, _ := newTFService(t)
	req := validTFReq()
	req.RequestId = ""
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty request_id: want InvalidArgument, got %v", err)
	}
	// The reject is clean: the same request with a request_id still registers.
	req.RequestId = "req-ok"
	if _, err := svc.RegisterToolFunction(context.Background(), req); err != nil {
		t.Fatalf("valid request_id should register: %v", err)
	}
}

// TestRegisterToolFunction_UnknownParameterTypeRejected covers the P2 identity-bearing enum
// gate: an out-of-range ParameterType (a forward protobuf client can send e.g. type 99)
// must be rejected fail-closed rather than serialized into tool_function_digest.
func TestRegisterToolFunction_UnknownParameterTypeRejected(t *testing.T) {
	svc, _ := newTFService(t)
	req := validTFReq()
	req.Spec.Parameters[0].Type = nfv1.ParameterType(99)
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown ParameterType: want InvalidArgument, got %v", err)
	}
	// Restoring a defined value makes the same request valid — only the unknown enum was rejected.
	req.Spec.Parameters[0].Type = nfv1.ParameterType_PARAMETER_TYPE_STRING
	if _, err := svc.RegisterToolFunction(context.Background(), req); err != nil {
		t.Fatalf("defined ParameterType should register: %v", err)
	}
}

// TestRegisterToolFunction_UnknownIntermediatePolicyRejected covers the P2 identity-bearing
// enum gate for IntermediateFilePolicyKind, which also enters tool_function_digest.
func TestRegisterToolFunction_UnknownIntermediatePolicyRejected(t *testing.T) {
	svc, _ := newTFService(t)
	req := validTFReq()
	req.Spec.IntermediateFilePolicies = []*nfv1.IntermediateFilePolicy{
		{PathOrPattern: "tmp/*", Policy: nfv1.IntermediateFilePolicyKind(99)},
	}
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown IntermediateFilePolicyKind: want InvalidArgument, got %v", err)
	}
	req.Spec.IntermediateFilePolicies[0].Policy = nfv1.IntermediateFilePolicyKind_INTERMEDIATE_FILE_POLICY_KIND_EPHEMERAL
	if _, err := svc.RegisterToolFunction(context.Background(), req); err != nil {
		t.Fatalf("defined IntermediateFilePolicyKind should register: %v", err)
	}
}

// TestRegisterToolFunction_UnknownProtoFieldRejected covers the identity-completeness gate:
// an unknown protobuf field anywhere in the identity-bearing spec (a newer-proto client)
// must be rejected before hashing, since the canonicalizer would otherwise omit it and let a
// semantically-different spec collide with an older tool_function_digest.
func TestRegisterToolFunction_UnknownProtoFieldRejected(t *testing.T) {
	// An unknown field: tag for field 50000 (varint) + value.
	unknown := protowire.AppendTag(nil, 50000, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 7)

	t.Run("top-level", func(t *testing.T) {
		svc, _ := newTFService(t)
		req := validTFReq()
		req.Spec.ProtoReflect().SetUnknown(protoreflect.RawFields(unknown))
		if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unknown top-level spec field: want InvalidArgument, got %v", err)
		}
	})

	t.Run("nested", func(t *testing.T) {
		svc, _ := newTFService(t)
		req := validTFReq()
		// Unknown field inside a nested repeated message (an input port) — exercises recursion.
		req.Spec.Inputs[0].ProtoReflect().SetUnknown(protoreflect.RawFields(unknown))
		if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unknown nested spec field: want InvalidArgument, got %v", err)
		}
	})

	t.Run("known-spec-still-registers", func(t *testing.T) {
		svc, _ := newTFService(t)
		if _, err := svc.RegisterToolFunction(context.Background(), validTFReq()); err != nil {
			t.Fatalf("a spec with no unknown fields must register: %v", err)
		}
	})
}

// TestRegisterToolFunction_UnknownPresentationFieldRejected covers the round-2 P2: an unknown
// protobuf field anywhere in ToolFunctionPresentation must be rejected before the presentation
// revision is hashed/persisted (the canonicalizer would otherwise silently drop it, producing
// a lossy or empty revision).
func TestRegisterToolFunction_UnknownPresentationFieldRejected(t *testing.T) {
	unknown := protowire.AppendTag(nil, 50000, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 7)

	t.Run("top-level", func(t *testing.T) {
		svc, _ := newTFService(t)
		req := validTFReq()
		req.Presentation.ProtoReflect().SetUnknown(protoreflect.RawFields(unknown))
		if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unknown presentation field: want InvalidArgument, got %v", err)
		}
	})

	t.Run("nested", func(t *testing.T) {
		svc, _ := newTFService(t)
		req := validTFReq()
		req.Presentation.OutputPortPresentations[0].ProtoReflect().SetUnknown(protoreflect.RawFields(unknown))
		if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unknown nested presentation field: want InvalidArgument, got %v", err)
		}
	})
}

// TestRegisterToolFunction_PresentEmptyMessageDistinctDigest covers the N1 digest gap: an
// explicitly-present-but-empty nested message (command: {}) must produce a different
// tool_function_digest than an absent one, so distinct authored specs cannot collide.
func TestRegisterToolFunction_PresentEmptyMessageDistinctDigest(t *testing.T) {
	svcAbsent, _ := newTFService(t)
	reqAbsent := validTFReq()
	reqAbsent.Spec.Command = nil // absent
	respAbsent := mustRegisterTF(t, svcAbsent, reqAbsent)

	svcEmpty, _ := newTFService(t)
	reqEmpty := validTFReq()
	reqEmpty.Spec.Command = &nfv1.CommandContract{} // present but empty
	respEmpty := mustRegisterTF(t, svcEmpty, reqEmpty)

	if respAbsent.GetToolFunctionDigest() == respEmpty.GetToolFunctionDigest() {
		t.Fatal("present-empty command must yield a different tool_function_digest than an absent command")
	}
}

// TestRegisterToolFunction_BaseToolSpecMustExist covers the round-4 base-existence contract:
// a well-formed base_tool_spec_digest that does not resolve to an authoritative
// ResolvedToolSpec is rejected NotFound with zero mutation; a present base registers.
func TestRegisterToolFunction_BaseToolSpecMustExist(t *testing.T) {
	t.Run("missing-base-not-found", func(t *testing.T) {
		cat := catalog.NewCatalogAt(t.TempDir())
		store, err := index.NewAt(t.TempDir())
		if err != nil {
			t.Fatalf("index.NewAt: %v", err)
		}
		svc := catalog.NewToolRegistryService(cat, store) // base deliberately NOT seeded
		req := validTFReq()
		if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.NotFound {
			t.Fatalf("absent base: want NotFound, got %v", err)
		}
		// Clean reject (zero mutation): after seeding the base, the same request registers
		// as a fresh record (no conflict/partial state from the rejected attempt).
		seedResolvedToolSpec(t, store, baseDigest)
		if _, err := svc.RegisterToolFunction(context.Background(), req); err != nil {
			t.Fatalf("after seeding base, registration should succeed: %v", err)
		}
	})

	t.Run("present-base-registers", func(t *testing.T) {
		svc, _ := newTFService(t) // seeds baseDigest
		if _, err := svc.RegisterToolFunction(context.Background(), validTFReq()); err != nil {
			t.Fatalf("present base should register: %v", err)
		}
	})

	t.Run("normalized-digest-resolves", func(t *testing.T) {
		// The base lookup uses the normalized (trim+lower) digest, matching how the base was
		// stored, so a case/whitespace-variant base still resolves.
		svc, _ := newTFService(t)
		req := validTFReq()
		req.BaseToolSpecDigest = "  " + strings.ToUpper(baseDigest) + "  "
		if _, err := svc.RegisterToolFunction(context.Background(), req); err != nil {
			t.Fatalf("normalized base should resolve: %v", err)
		}
	})
}
