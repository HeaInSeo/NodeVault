package catalog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// RegisterToolFunction DC-R1-NV-C1 cutover tests: w2-set-v1 canonicalizer, version selection,
// full request basis receipt, UNKNOWN_LEGACY. Matrix ids refer to GR-NV-F011 M1–M19.

// setLikeTFReq is a w2-set-v1 request with two entries in every one of the nine set-like
// fields (and two command arguments).
func setLikeTFReq() *nfv1.RegisterToolFunctionRequest {
	req := validTFReq()
	req.Spec.Command = &nfv1.CommandContract{
		Executable: "bwa",
		Arguments:  []string{"mem", "{param.threads}", "{param.k}"},
		Environment: []*nfv1.EnvironmentEntry{
			{Name: "A", Source: "secret:a"}, {Name: "B", Source: "secret:b"},
		},
		SuccessExitCodes: []int32{0, 2},
	}
	req.Spec.Inputs = []*nfv1.FunctionPortSpec{
		{Name: "reads", DataFormat: fmtFastq, CompanionFiles: []string{"x.idx", "a.idx"}},
		{Name: "ref", DataFormat: "fasta"},
	}
	req.Spec.Outputs = []*nfv1.FunctionPortSpec{
		{Name: portAligned, DataFormat: "bam"},
		{Name: "log", DataFormat: "txt"},
	}
	req.Spec.Parameters = []*nfv1.ParameterSpec{
		{Name: "threads", Type: nfv1.ParameterType_PARAMETER_TYPE_INTEGER},
		{Name: "k", Type: nfv1.ParameterType_PARAMETER_TYPE_INTEGER},
	}
	req.Spec.IntermediateFilePolicies = []*nfv1.IntermediateFilePolicy{
		{PathOrPattern: "tmp/*", Policy: nfv1.IntermediateFilePolicyKind_INTERMEDIATE_FILE_POLICY_KIND_EPHEMERAL},
		{PathOrPattern: "cache/*", Policy: nfv1.IntermediateFilePolicyKind_INTERMEDIATE_FILE_POLICY_KIND_CACHE},
	}
	req.Spec.ExecutionEnvironment = &nfv1.ExecutionEnvironmentSpec{
		WritablePaths:        []string{"/tmp", "/work"},
		RequiredCapabilities: []string{"CAP_SYS_PTRACE", "CAP_NET_RAW"},
	}
	return req
}

func registerDigest(t *testing.T, req *nfv1.RegisterToolFunctionRequest) string {
	t.Helper()
	svc, _ := newTFService(t)
	return mustRegisterTF(t, svc, req).GetToolFunctionDigest()
}

// M2: swapping the order inside each of the nine set-like fields keeps the v1 digest.
func TestW2SetV1_SetLikePermutationsSameDigest(t *testing.T) {
	base := registerDigest(t, setLikeTFReq())
	swaps := map[string]func(*nfv1.ToolFunctionSpec){
		"environment": func(s *nfv1.ToolFunctionSpec) {
			e := s.Command.Environment
			e[0], e[1] = e[1], e[0]
		},
		"inputs":  func(s *nfv1.ToolFunctionSpec) { s.Inputs[0], s.Inputs[1] = s.Inputs[1], s.Inputs[0] },
		"outputs": func(s *nfv1.ToolFunctionSpec) { s.Outputs[0], s.Outputs[1] = s.Outputs[1], s.Outputs[0] },
		"parameters": func(s *nfv1.ToolFunctionSpec) {
			s.Parameters[0], s.Parameters[1] = s.Parameters[1], s.Parameters[0]
		},
		"success_exit_codes": func(s *nfv1.ToolFunctionSpec) {
			c := s.Command.SuccessExitCodes
			c[0], c[1] = c[1], c[0]
		},
		"companion_files": func(s *nfv1.ToolFunctionSpec) {
			f := s.Inputs[0].CompanionFiles
			f[0], f[1] = f[1], f[0]
		},
		"intermediate_file_policies": func(s *nfv1.ToolFunctionSpec) {
			p := s.IntermediateFilePolicies
			p[0], p[1] = p[1], p[0]
		},
		"writable_paths": func(s *nfv1.ToolFunctionSpec) {
			w := s.ExecutionEnvironment.WritablePaths
			w[0], w[1] = w[1], w[0]
		},
		"required_capabilities": func(s *nfv1.ToolFunctionSpec) {
			c := s.ExecutionEnvironment.RequiredCapabilities
			c[0], c[1] = c[1], c[0]
		},
	}
	if len(swaps) != 9 {
		t.Fatalf("expected the nine set-like fields, got %d", len(swaps))
	}
	for name, swap := range swaps {
		t.Run(name, func(t *testing.T) {
			req := setLikeTFReq()
			swap(req.Spec)
			if got := registerDigest(t, req); got != base {
				t.Fatalf("%s permutation changed the v1 digest", name)
			}
		})
	}
}

// M3: command.arguments stays order-sensitive under v1.
func TestW2SetV1_ArgumentsOrderSensitive(t *testing.T) {
	base := registerDigest(t, setLikeTFReq())
	req := setLikeTFReq()
	a := req.Spec.Command.Arguments
	a[1], a[2] = a[2], a[1]
	if registerDigest(t, req) == base {
		t.Fatal("reordering command.arguments must change the v1 digest")
	}
}

// M4: an exact duplicate in each of the nine set-like fields is InvalidArgument with zero mutation.
func TestW2SetV1_ExactDuplicatesRejected(t *testing.T) {
	dups := map[string]func(*nfv1.ToolFunctionSpec){
		"environment": func(s *nfv1.ToolFunctionSpec) {
			s.Command.Environment = append(s.Command.Environment, &nfv1.EnvironmentEntry{Name: "A", Source: "secret:a"})
		},
		"inputs": func(s *nfv1.ToolFunctionSpec) {
			s.Inputs = append(s.Inputs, &nfv1.FunctionPortSpec{Name: "ref", DataFormat: "fasta"})
		},
		"outputs": func(s *nfv1.ToolFunctionSpec) {
			s.Outputs = append(s.Outputs, &nfv1.FunctionPortSpec{Name: "log", DataFormat: "txt"})
		},
		"parameters": func(s *nfv1.ToolFunctionSpec) {
			s.Parameters = append(s.Parameters,
				&nfv1.ParameterSpec{Name: "k", Type: nfv1.ParameterType_PARAMETER_TYPE_INTEGER})
		},
		"success_exit_codes": func(s *nfv1.ToolFunctionSpec) {
			s.Command.SuccessExitCodes = append(s.Command.SuccessExitCodes, 0)
		},
		"companion_files": func(s *nfv1.ToolFunctionSpec) {
			s.Inputs[0].CompanionFiles = append(s.Inputs[0].CompanionFiles, "a.idx")
		},
		"intermediate_file_policies": func(s *nfv1.ToolFunctionSpec) {
			s.IntermediateFilePolicies = append(s.IntermediateFilePolicies, &nfv1.IntermediateFilePolicy{
				PathOrPattern: "tmp/*", Policy: nfv1.IntermediateFilePolicyKind_INTERMEDIATE_FILE_POLICY_KIND_EPHEMERAL,
			})
		},
		"writable_paths": func(s *nfv1.ToolFunctionSpec) {
			s.ExecutionEnvironment.WritablePaths = append(s.ExecutionEnvironment.WritablePaths, "/tmp")
		},
		"required_capabilities": func(s *nfv1.ToolFunctionSpec) {
			s.ExecutionEnvironment.RequiredCapabilities = append(s.ExecutionEnvironment.RequiredCapabilities, "CAP_NET_RAW")
		},
	}
	if len(dups) != 9 {
		t.Fatalf("expected the nine set-like fields, got %d", len(dups))
	}
	for name, dup := range dups {
		t.Run(name, func(t *testing.T) {
			svc, store := newTFService(t)
			req := setLikeTFReq()
			dup(req.Spec)
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("duplicate %s: want InvalidArgument, got %v", name, err)
			}
			if _, err := store.GetToolFunctionRequestRecord(req.GetRequestId()); !errors.Is(err, index.ErrNotFound) {
				t.Fatalf("duplicate %s mutated the store: %v", name, err)
			}
		})
	}
}

// M5: string content is exact — no path/whitespace/case normalization.
func TestW2SetV1_StringExactness(t *testing.T) {
	base := registerDigest(t, setLikeTFReq())
	for name, mut := range map[string]func(*nfv1.ToolFunctionSpec){
		"trailing slash": func(s *nfv1.ToolFunctionSpec) { s.ExecutionEnvironment.WritablePaths[0] = "/tmp/" },
		"padded":         func(s *nfv1.ToolFunctionSpec) { s.Command.Executable = " bwa " },
	} {
		t.Run(name, func(t *testing.T) {
			req := setLikeTFReq()
			mut(req.Spec)
			if registerDigest(t, req) == base {
				t.Fatalf("%s variant must be a distinct identity", name)
			}
		})
	}
	// A non-canonical capability spelling is rejected, never normalized.
	svc, _ := newTFService(t)
	req := setLikeTFReq()
	req.Spec.ExecutionEnvironment.RequiredCapabilities[0] = "cap_sys_ptrace"
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("lowercase capability: want InvalidArgument, got %v", err)
	}
}

// M6: an empty/invalid repeated entry is rejected, not erased by semantic-empty omission.
func TestW2SetV1_EmptyRepeatedEntriesRejected(t *testing.T) {
	for name, mut := range map[string]func(*nfv1.ToolFunctionSpec){
		"env name":         func(s *nfv1.ToolFunctionSpec) { s.Command.Environment[0].Name = "" },
		"companion file":   func(s *nfv1.ToolFunctionSpec) { s.Inputs[0].CompanionFiles[0] = " " },
		"policy pattern":   func(s *nfv1.ToolFunctionSpec) { s.IntermediateFilePolicies[0].PathOrPattern = "" },
		"writable path":    func(s *nfv1.ToolFunctionSpec) { s.ExecutionEnvironment.WritablePaths[0] = "" },
		"capability empty": func(s *nfv1.ToolFunctionSpec) { s.ExecutionEnvironment.RequiredCapabilities[0] = "" },
		"pattern clash": func(s *nfv1.ToolFunctionSpec) {
			s.IntermediateFilePolicies[1].PathOrPattern = s.IntermediateFilePolicies[0].PathOrPattern
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, store := newTFService(t)
			req := setLikeTFReq()
			mut(req.Spec)
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: want InvalidArgument, got %v", name, err)
			}
			if _, err := store.GetToolFunctionRequestRecord(req.GetRequestId()); !errors.Is(err, index.ErrNotFound) {
				t.Fatalf("%s mutated the store: %v", name, err)
			}
		})
	}
}

// M7: the same request_id with only presentation / validation_policy / environment_hints
// changed is a conflict with zero mutation (no new receipt or presentation revision).
func TestW2SetV1_SameRequestIDEnvelopeChangeConflicts(t *testing.T) {
	for name, mut := range map[string]func(*nfv1.RegisterToolFunctionRequest){
		"presentation": func(r *nfv1.RegisterToolFunctionRequest) { r.Presentation.Label = "other" },
		"validation_policy": func(r *nfv1.RegisterToolFunctionRequest) {
			r.ValidationPolicy = &nfv1.ToolFunctionValidationPolicy{
				ExpectedResults: []*nfv1.ExpectedResult{{OutputPortName: portAligned, ExpectedValueOrRule: "nonempty"}},
			}
		},
		"environment_hints": func(r *nfv1.RegisterToolFunctionRequest) {
			r.EnvironmentHints = &nfv1.ToolFunctionEnvironmentHints{SupportedPlatforms: []string{"linux/amd64"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, store := newTFService(t)
			first := mustRegisterTF(t, svc, validTFReq())
			req := validTFReq()
			mut(req)
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.AlreadyExists {
				t.Fatalf("%s change under same request_id: want AlreadyExists, got %v", name, err)
			}
			rec, err := store.GetToolFunctionRequestRecord(req.GetRequestId())
			if err != nil || rec.PresentationRevisionID != first.GetPresentationRevisionId() {
				t.Fatalf("receipt changed after conflict: %+v err=%v", rec, err)
			}
		})
	}
}

// M8: the same request_id replayed under a different canonicalizer version is a conflict.
func TestW2SetV1_SameRequestIDVersionChangeConflicts(t *testing.T) {
	svc, _ := newTFService(t)
	mustRegisterTF(t, svc, validTFReq())
	req := validTFReq()
	req.CanonicalizationVersion = index.CanonicalizationLegacyOrderV0
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("version change under same request_id: want AlreadyExists, got %v", err)
	}
}

// M9: a new request_id without a version, or with legacy-order-v0, is rejected — no default.
// M13: an unsupported version is rejected fail-closed.
func TestW2SetV1_NewRequestVersionGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    codes.Code
	}{
		{"", codes.InvalidArgument},
		{index.CanonicalizationLegacyOrderV0, codes.FailedPrecondition},
		{"w2-set-v2", codes.InvalidArgument},
	} {
		t.Run("version="+tc.version, func(t *testing.T) {
			svc, store := newTFService(t)
			req := validTFReq()
			req.CanonicalizationVersion = tc.version
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if _, err := store.GetToolFunctionRequestRecord(req.GetRequestId()); !errors.Is(err, index.ErrNotFound) {
				t.Fatalf("rejected version mutated the store: %v", err)
			}
		})
	}
}

// The receipt records the selected version and full basis; the response projects the version.
func TestW2SetV1_ReceiptAndResponseVersion(t *testing.T) {
	svc, store := newTFService(t)
	resp := mustRegisterTF(t, svc, validTFReq())
	if resp.GetCanonicalizationVersion() != index.CanonicalizationW2SetV1 {
		t.Fatalf("response canonicalization_version = %q", resp.GetCanonicalizationVersion())
	}
	rec, err := store.GetToolFunctionRequestRecord("req-1")
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if !rec.BasisKnown() || rec.CanonicalizationVersion != index.CanonicalizationW2SetV1 {
		t.Fatalf("receipt lacks version/basis: %+v", rec)
	}
	tf, err := store.GetToolFunctionByCasHash(resp.GetCasHash())
	if err != nil || tf.CanonicalizationVersion != index.CanonicalizationW2SetV1 {
		t.Fatalf("record derivation version: %+v err=%v", tf, err)
	}
	// An identical replay returns the same receipt result.
	again := mustRegisterTF(t, svc, validTFReq())
	if again.GetCasHash() != resp.GetCasHash() || again.GetCanonicalizationVersion() != index.CanonicalizationW2SetV1 {
		t.Fatalf("identical replay diverged: %+v vs %+v", again, resp)
	}
}

// legacyCatalogService opens a store over a pre-cutover index holding one runnable record
// (with this request's exact identity) and its basis-less receipt.
func legacyCatalogService(t *testing.T, legacyCas string) (*catalog.ToolRegistryService, *index.Store) {
	t.Helper()
	dir := t.TempDir()
	legacy := `{"schema_version":6,"entries":[],
"resolved_tool_specs":[{"tool_spec_digest":"` + baseDigest + `"}],
"registered_tool_functions":[{"cas_hash":"` + legacyCas + `","tool_function_digest":"legacy-tfd",
"function_image_digest":"legacy-img","artifact_kind":"tool_function","request_id":"req-1",
"lifecycle_phase":"Active","integrity_health":"Partial","registered_at":"2026-09-01T00:00:00Z"}],
"tool_function_request_records":[{"request_id":"req-1","cas_hash":"` + legacyCas + `",
"created_at":"2026-09-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, "vault-index.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed legacy index: %v", err)
	}
	store, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	return catalog.NewToolRegistryService(catalog.NewCatalogAt(t.TempDir()), store), store
}

// M10: replaying a legacy request_id without a stored basis is FailedPrecondition (UNKNOWN),
// with zero mutation — under any requested version.
func TestW2SetV1_LegacyReplayUnknown(t *testing.T) {
	for _, version := range []string{"", index.CanonicalizationW2SetV1} {
		t.Run("version="+version, func(t *testing.T) {
			svc, store := legacyCatalogService(t, "legacy-cas")
			req := validTFReq()
			req.CanonicalizationVersion = version
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("legacy replay: want FailedPrecondition, got %v", err)
			}
			rec, err := store.GetToolFunctionRequestRecord("req-1")
			if err != nil || rec.BasisKnown() {
				t.Fatalf("legacy receipt changed: %+v err=%v", rec, err)
			}
		})
	}
}

// M11: a new request_id whose v1 cas_hash coincides with an UNKNOWN_LEGACY record cannot prove
// the full basis, so the legacy identity is neither reused nor relabeled (HOLD).
func TestW2SetV1_HashCoincidesWithLegacyHeld(t *testing.T) {
	probeSvc, _ := newTFService(t)
	cas := mustRegisterTF(t, probeSvc, validTFReq()).GetCasHash()

	svc, store := legacyCatalogService(t, cas)
	req := validTFReq()
	req.RequestId = "req-new"
	if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("coinciding legacy hash: want FailedPrecondition, got %v", err)
	}
	if _, err := store.GetToolFunctionRequestRecord("req-new"); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("held registration recorded a receipt: %v", err)
	}
	tf, err := store.GetToolFunctionByCasHash(cas)
	if err != nil || tf.CanonicalizationVersion != "" || tf.ToolFunctionDigest != "legacy-tfd" {
		t.Fatalf("legacy record relabeled: %+v err=%v", tf, err)
	}
}

// N6: a new request_id with identical content and envelope reuses a v1 identity; a different
// envelope (presentation/policy/hints) cannot silently reuse it.
func TestW2SetV1_NewRequestIDReuseNeedsSameEnvelope(t *testing.T) {
	svc, _ := newTFService(t)
	first := mustRegisterTF(t, svc, validTFReq())

	same := validTFReq()
	same.RequestId = "req-same"
	if got := mustRegisterTF(t, svc, same); got.GetCasHash() != first.GetCasHash() {
		t.Fatal("identical content+envelope must reuse the identity")
	}

	diff := validTFReq()
	diff.RequestId = "req-diff"
	diff.EnvironmentHints = &nfv1.ToolFunctionEnvironmentHints{SupportedPlatforms: []string{"linux/arm64"}}
	if _, err := svc.RegisterToolFunction(context.Background(), diff); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("different envelope reuse: want FailedPrecondition, got %v", err)
	}
}

// M13: unknown fields and undefined enums in the digest-out envelope are rejected fail-closed.
func TestW2SetV1_UnknownEnvelopeRejected(t *testing.T) {
	unknown := protowire.AppendTag(nil, 50000, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 7)
	for name, mut := range map[string]func(*nfv1.RegisterToolFunctionRequest){
		"policy unknown field": func(r *nfv1.RegisterToolFunctionRequest) {
			r.ValidationPolicy = &nfv1.ToolFunctionValidationPolicy{}
			r.ValidationPolicy.ProtoReflect().SetUnknown(protoreflect.RawFields(unknown))
		},
		"hints unknown nested field": func(r *nfv1.RegisterToolFunctionRequest) {
			r.EnvironmentHints = &nfv1.ToolFunctionEnvironmentHints{EnforcedResources: &nfv1.ResourceContract{}}
			r.EnvironmentHints.EnforcedResources.ProtoReflect().SetUnknown(protoreflect.RawFields(unknown))
		},
		"observation level": func(r *nfv1.RegisterToolFunctionRequest) {
			r.ValidationPolicy = &nfv1.ToolFunctionValidationPolicy{
				ValidationRequirements: &nfv1.ValidationRequirements{MinimumObservationLevel: nfv1.ObservationLevel(42)},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newTFService(t)
			req := validTFReq()
			mut(req)
			if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: want InvalidArgument, got %v", name, err)
			}
		})
	}
}
