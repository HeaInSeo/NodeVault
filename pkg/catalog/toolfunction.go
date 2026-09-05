// Package catalog — RegisterToolFunction (issue #19 W2).
//
// This file implements the runnable second-image ToolFunction registration path.
// Unlike the first-image build path (RegisterTool, raw_spec string), the function
// image is authored in NodeKit as typed contracts, so NodeVault receives them typed
// and OWNS canonicalization + identity (N3): it serializes its own canonical JSON and
// computes both digests here. Callers never recompute identity.
//
// Identity (nodevault.proto §4.2 / D-19-3, W1-Q1 RESPONSE BOUNDARY FINAL):
//
//	tool_function_digest = SHA256(canonical_json({ base_tool_spec_digest, spec }))
//	cas_hash             = SHA256(canonical_json(ToolFunctionArtifactSpec{
//	                          tool_function_digest, function_image_digest }))
//
// presentation / validation_policy / environment_hints are digest-OUT: they never
// affect tool_function_digest or cas_hash. Presentation is persisted as a separate
// content-addressed revision (D-19-4).
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// jsonKeyName is the canonical-JSON key shared by several named sub-entities
// (ports, parameters, environment entries); defined once to keep canonicalization
// consistent.
const jsonKeyName = "name"

// baseToolSpecDigestRE matches a NodeVault ToolSpec digest: a bare 64-character
// lowercase hex SHA-256 (resolve.ToolSpecDigest's form). base_tool_spec_digest enters
// an immutable ToolFunction identity + lineage, so an arbitrary string is rejected to
// prevent permanently malformed/dangling lineage.
var baseToolSpecDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RegisterToolFunction validates a typed ToolFunctionSpec declaration, computes the
// NodeVault-owned tool_function_digest and cas_hash over its own canonical JSON,
// durably registers the runnable record (+ optional presentation revision) atomically,
// and returns the identity. It is idempotent by request_id and by content (cas_hash);
// a new successful runnable record starts lifecycle Active and re-registration never
// resurrects a Retracted/Deleted record.
func (s *ToolRegistryService) RegisterToolFunction(
	_ context.Context, req *nfv1.RegisterToolFunctionRequest,
) (*nfv1.RegisterToolFunctionResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "tool registry unavailable")
	}
	// request_id is the idempotency key (nodevault.proto): without it a lost response
	// followed by a retry whose spec/image changed would be accepted as a second runnable
	// record instead of being detected as reuse of the same operation. Reject empty before
	// any mutation, matching the analogous SubmitToolBuild contract.
	if req.GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id is required (idempotency key)")
	}
	// Canonicalize the two identity-bearing digest inputs before they enter any
	// preimage (N2/N3, NodeVault owns identity): trim surrounding whitespace and
	// lowercase, so case- or whitespace-variant spellings of the same digest converge
	// to one cas_hash/tool_function_digest instead of forking identity.
	baseToolSpecDigest := strings.ToLower(strings.TrimSpace(req.GetBaseToolSpecDigest()))
	imageDigest := strings.ToLower(strings.TrimSpace(req.GetImageDigest()))
	if baseToolSpecDigest == "" {
		return nil, status.Error(codes.InvalidArgument, "base_tool_spec_digest is required")
	}
	if !baseToolSpecDigestRE.MatchString(baseToolSpecDigest) {
		return nil, status.Error(codes.InvalidArgument,
			"base_tool_spec_digest must be a 64-character lowercase hex sha256 digest")
	}
	if !resolve.IsSHA256Digest(imageDigest) {
		return nil, status.Error(codes.InvalidArgument, "image_digest must be a pinned sha256:<64 hex> digest")
	}
	if req.GetSpec() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	// The entire spec is identity-bearing (it feeds tool_function_digest), but the
	// canonicalizer only serializes the currently-known fields. A client built from a newer
	// proto could send an added field that survives as unknown wire bytes and would be
	// omitted from the digest, so two semantically different specs would collide on one
	// identity. Reject unknown fields anywhere in the spec subtree before hashing, fail-closed
	// (same spirit as the cardinality/enum gates: no uninterpretable content enters identity).
	if err := rejectUnknownSpecFields(req.GetSpec()); err != nil {
		return nil, err
	}
	if err := validateToolFunctionSpec(req.GetSpec()); err != nil {
		return nil, err
	}
	if err := validateToolFunctionPresentation(req.GetPresentation(), req.GetSpec()); err != nil {
		return nil, err
	}
	if err := validateToolFunctionValidationPolicy(req.GetValidationPolicy(), req.GetSpec()); err != nil {
		return nil, err
	}

	// NodeVault-owned identity (N3): canonical JSON + SHA256, over frozen preimages.
	toolFunctionDigest := computeToolFunctionDigest(baseToolSpecDigest, req.GetSpec())
	casHash := computeToolFunctionCasHash(toolFunctionDigest, imageDigest)

	// Presentation is digest-out; persist it as a content-addressed revision.
	presentationRevID, rev := buildPresentationRevision(req.GetPresentation(), casHash)

	rec := index.RegisteredToolFunction{
		CasHash:                casHash,
		ToolFunctionDigest:     toolFunctionDigest,
		FunctionImageDigest:    imageDigest,
		BaseToolSpecDigest:     baseToolSpecDigest,
		ArtifactKind:           index.KindToolFunction,
		PresentationRevisionID: presentationRevID,
		RequestID:              req.GetRequestId(),
		LifecyclePhase:         index.PhaseActive,
		IntegrityHealth:        index.HealthPartial, // Partial until reconcile observes Harbor
	}

	stored, _, err := s.store.RegisterToolFunctionAtomic(req.GetRequestId(), rec, rev)
	if err != nil {
		if errors.Is(err, index.ErrToolFunctionRequestConflict) {
			return nil, status.Errorf(codes.AlreadyExists,
				"request_id %q was already used for different content", req.GetRequestId())
		}
		return nil, status.Errorf(codes.Internal, "register tool function: %v", err)
	}

	return &nfv1.RegisterToolFunctionResponse{
		ToolFunctionDigest:     stored.ToolFunctionDigest,
		PresentationRevisionId: stored.PresentationRevisionID,
		CasHash:                stored.CasHash,
	}, nil
}

// ── validation (fail-closed) ──────────────────────────────────────────────────

// rejectUnknownSpecFields fails closed if the spec, or any message nested within it,
// carries unknown protobuf fields (forward-compatible wire bytes from a newer client). Such
// fields are invisible to the canonicalizer and would otherwise be silently excluded from
// tool_function_digest, letting a newer, semantically-different spec collide with an older
// identity. It walks the whole spec subtree via protoreflect and checks GetUnknown() at each
// message level.
func rejectUnknownSpecFields(spec *nfv1.ToolFunctionSpec) error {
	if spec == nil {
		return nil
	}
	if name, found := firstUnknownField(spec.ProtoReflect()); found {
		return status.Errorf(codes.InvalidArgument,
			"spec contains unknown protobuf field(s) in %s; identity would be incomplete", name)
	}
	return nil
}

// firstUnknownField returns the descriptor name of the first message in the subtree that
// carries unknown fields, walking populated message/list/map fields recursively. Unknown
// fields are captured per-message by GetUnknown(), so checking each reachable message level
// covers the entire tree.
func firstUnknownField(m protoreflect.Message) (string, bool) {
	if m == nil || !m.IsValid() {
		return "", false
	}
	if len(m.GetUnknown()) > 0 {
		return string(m.Descriptor().FullName()), true
	}
	name := ""
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					if n, ok := firstUnknownField(mv.Message()); ok {
						name, found = n, true
						return false
					}
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					if n, ok := firstUnknownField(l.Get(i).Message()); ok {
						name, found = n, true
						break
					}
				}
			}
		case fd.Message() != nil:
			if n, ok := firstUnknownField(v.Message()); ok {
				name, found = n, true
			}
		}
		return !found
	})
	return name, found
}

func validateToolFunctionSpec(spec *nfv1.ToolFunctionSpec) error {
	if err := validatePortCardinality(spec.GetInputs()); err != nil {
		return err
	}
	if err := validatePortCardinality(spec.GetOutputs()); err != nil {
		return err
	}
	if err := checkUniquePortNames("input", spec.GetInputs()); err != nil {
		return err
	}
	if err := checkUniquePortNames("output", spec.GetOutputs()); err != nil {
		return err
	}
	if err := checkUniqueParameterNames(spec.GetParameters()); err != nil {
		return err
	}
	if err := validateParameterTypes(spec.GetParameters()); err != nil {
		return err
	}
	return validateIntermediateFilePolicyKinds(spec.GetIntermediateFilePolicies())
}

// validateParameterTypes rejects any ParameterSpec.type whose numeric value is not a
// defined ParameterType enum member. Like the cardinality gate, this is fail-closed with
// zero persistent mutation: a forward protobuf client can send e.g. ParameterType(99),
// and the canonicalizer would otherwise serialize that uninterpretable number into
// tool_function_digest, minting a durable identity for a declaration the current contract
// cannot interpret. Allowlist by the generated enum name table (which includes the
// UNSPECIFIED 0 value; 0 is omitted from the digest, so it is harmless).
func validateParameterTypes(params []*nfv1.ParameterSpec) error {
	for _, p := range params {
		if _, ok := nfv1.ParameterType_name[int32(p.GetType())]; !ok {
			return status.Errorf(codes.InvalidArgument,
				"unknown parameter type %d (parameter %q)", int32(p.GetType()), p.GetName())
		}
	}
	return nil
}

// validateIntermediateFilePolicyKinds rejects any IntermediateFilePolicy.policy whose
// numeric value is not a defined IntermediateFilePolicyKind enum member, for the same
// reason as validateParameterTypes: the policy value enters tool_function_digest.
func validateIntermediateFilePolicyKinds(policies []*nfv1.IntermediateFilePolicy) error {
	for _, p := range policies {
		if _, ok := nfv1.IntermediateFilePolicyKind_name[int32(p.GetPolicy())]; !ok {
			return status.Errorf(codes.InvalidArgument,
				"unknown intermediate file policy %d (path %q)", int32(p.GetPolicy()), p.GetPathOrPattern())
		}
	}
	return nil
}

// validatePortCardinality enforces the approved cardinality contract: only
// CARDINALITY_UNSPECIFIED (omitted from canonical JSON, never normalized to SINGLE)
// and explicit CARDINALITY_SINGLE are allowed on the current single-capability path.
// CARDINALITY_MULTIPLE and any unknown/out-of-range enum value (a protobuf client can
// send e.g. Cardinality(99)) are rejected fail-closed with zero persistent mutation,
// so no uninterpretable cardinality ever enters the canonical digest.
func validatePortCardinality(ports []*nfv1.FunctionPortSpec) error {
	for _, p := range ports {
		switch p.GetCardinality() {
		case nfv1.Cardinality_CARDINALITY_UNSPECIFIED, nfv1.Cardinality_CARDINALITY_SINGLE:
			// allowed
		case nfv1.Cardinality_CARDINALITY_MULTIPLE:
			return status.Errorf(codes.InvalidArgument,
				"CARDINALITY_MULTIPLE is not supported (port %q)", p.GetName())
		default:
			return status.Errorf(codes.InvalidArgument,
				"unknown cardinality %d (port %q)", int32(p.GetCardinality()), p.GetName())
		}
	}
	return nil
}

func checkUniquePortNames(kind string, ports []*nfv1.FunctionPortSpec) error {
	seen := make(map[string]struct{}, len(ports))
	for _, p := range ports {
		name := p.GetName()
		if strings.TrimSpace(name) == "" {
			return status.Errorf(codes.InvalidArgument, "%s port name must not be empty", kind)
		}
		if _, dup := seen[name]; dup {
			return status.Errorf(codes.InvalidArgument, "duplicate %s port name %q", kind, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func checkUniqueParameterNames(params []*nfv1.ParameterSpec) error {
	seen := make(map[string]struct{}, len(params))
	for _, p := range params {
		name := p.GetName()
		if strings.TrimSpace(name) == "" {
			return status.Error(codes.InvalidArgument, "parameter name must not be empty")
		}
		if _, dup := seen[name]; dup {
			return status.Errorf(codes.InvalidArgument, "duplicate parameter name %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validateToolFunctionPresentation enforces the documented OutputPortPresentation
// referential integrity (nodevault.proto): each port_name must match exactly one
// declared output port, and duplicate port_name entries are forbidden.
func validateToolFunctionPresentation(pres *nfv1.ToolFunctionPresentation, spec *nfv1.ToolFunctionSpec) error {
	if pres == nil {
		return nil
	}
	outputs := outputPortNameSet(spec)
	seen := make(map[string]struct{}, len(pres.GetOutputPortPresentations()))
	for _, opp := range pres.GetOutputPortPresentations() {
		name := opp.GetPortName()
		if strings.TrimSpace(name) == "" {
			return status.Error(codes.InvalidArgument, "output_port_presentation.port_name must not be empty")
		}
		if _, ok := outputs[name]; !ok {
			return status.Errorf(codes.InvalidArgument,
				"output_port_presentation.port_name %q does not match any declared output port", name)
		}
		if _, dup := seen[name]; dup {
			return status.Errorf(codes.InvalidArgument,
				"duplicate output_port_presentation for port %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validateToolFunctionValidationPolicy rejects a policy that references an output port
// the spec does not declare (a policy/spec conflict): each ExpectedResult.
// output_port_name, when set, must match a declared output port.
func validateToolFunctionValidationPolicy(vp *nfv1.ToolFunctionValidationPolicy, spec *nfv1.ToolFunctionSpec) error {
	if vp == nil {
		return nil
	}
	outputs := outputPortNameSet(spec)
	for _, er := range vp.GetExpectedResults() {
		name := er.GetOutputPortName()
		if name == "" {
			continue
		}
		if _, ok := outputs[name]; !ok {
			return status.Errorf(codes.InvalidArgument,
				"validation_policy expected_result output_port_name %q does not match any declared output port", name)
		}
	}
	return nil
}

func outputPortNameSet(spec *nfv1.ToolFunctionSpec) map[string]struct{} {
	out := make(map[string]struct{})
	for _, p := range spec.GetOutputs() {
		out[p.GetName()] = struct{}{}
	}
	return out
}

// ── identity: NodeVault-owned canonical JSON + SHA256 ─────────────────────────

func computeToolFunctionDigest(baseToolSpecDigest string, spec *nfv1.ToolFunctionSpec) string {
	return canonicalSHA256(map[string]any{
		"base_tool_spec_digest": baseToolSpecDigest,
		"spec":                  canonicalToolFunctionSpec(spec),
	})
}

func computeToolFunctionCasHash(toolFunctionDigest, functionImageDigest string) string {
	return canonicalSHA256(map[string]any{
		"tool_function_digest":  toolFunctionDigest,
		"function_image_digest": functionImageDigest,
	})
}

// canonicalSHA256 marshals v to JSON (encoding/json sorts object keys, giving one
// canonical byte form — N2) and returns the lowercase hex SHA256, matching the bare-
// hex casHash convention used by catalog.SaveWithCasHash.
func canonicalSHA256(v any) string {
	// The preimages are plain map[string]any / []any / scalar trees that cannot fail
	// to marshal; a marshal error here would be a programming error, so fall back to a
	// non-colliding sentinel over the Go rendering rather than silently hashing "".
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("nodevault.toolfunction.canonicalization_error\x00" + errValue(err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func errValue(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// canonicalToolFunctionSpec renders the digest-input ToolFunctionSpec as a canonical
// map tree: unspecified/empty fields are omitted (N1), repeated fields preserve their
// authored order (order is identity-bearing), and enums are emitted as their integer
// value only when non-zero (CARDINALITY_UNSPECIFIED etc. are omitted, never rewritten
// to SINGLE).
func canonicalToolFunctionSpec(spec *nfv1.ToolFunctionSpec) map[string]any {
	m := map[string]any{}
	if spec == nil {
		return m
	}
	putMap(m, "command", canonicalCommand(spec.GetCommand()))
	putList(m, "inputs", canonicalPorts(spec.GetInputs()))
	putList(m, "outputs", canonicalPorts(spec.GetOutputs()))
	putList(m, "parameters", canonicalParameters(spec.GetParameters()))
	putList(m, "intermediate_file_policies", canonicalIntermediatePolicies(spec.GetIntermediateFilePolicies()))
	putMap(m, "execution_environment", canonicalExecEnv(spec.GetExecutionEnvironment()))
	return m
}

func canonicalCommand(c *nfv1.CommandContract) map[string]any {
	m := map[string]any{}
	if c == nil {
		return m
	}
	putStr(m, "executable", c.GetExecutable())
	putStrList(m, "arguments", c.GetArguments())
	putStr(m, "working_directory", c.GetWorkingDirectory())
	env := make([]any, 0, len(c.GetEnvironment()))
	for _, e := range c.GetEnvironment() {
		em := map[string]any{}
		putStr(em, jsonKeyName, e.GetName())
		putStr(em, "source", e.GetSource())
		env = append(env, em)
	}
	putList(m, "environment", env)
	if codesList := c.GetSuccessExitCodes(); len(codesList) > 0 {
		ints := make([]any, len(codesList))
		for i, v := range codesList {
			ints[i] = v
		}
		m["success_exit_codes"] = ints
	}
	if tp := c.GetTimeoutPolicy(); tp != nil {
		tm := map[string]any{}
		putInt(tm, "soft_seconds", int64(tp.GetSoftSeconds()))
		putInt(tm, "hard_seconds", int64(tp.GetHardSeconds()))
		putMap(m, "timeout_policy", tm)
	}
	return m
}

//nolint:dupl // canonicalPorts and canonicalParameters are parallel canonicalizers over different proto field sets.
func canonicalPorts(ports []*nfv1.FunctionPortSpec) []any {
	list := make([]any, 0, len(ports))
	for _, p := range ports {
		pm := map[string]any{}
		putStr(pm, jsonKeyName, p.GetName())
		putStr(pm, "data_format", p.GetDataFormat())
		putInt(pm, "cardinality", int64(p.GetCardinality()))
		putBool(pm, "required", p.GetRequired())
		putStr(pm, "path_or_glob", p.GetPathOrGlob())
		putStr(pm, "path_placement_rule", p.GetPathPlacementRule())
		putStrList(pm, "companion_files", p.GetCompanionFiles())
		putStr(pm, "completion_check", p.GetCompletionCheck())
		list = append(list, pm)
	}
	return list
}

//nolint:dupl // canonicalParameters and canonicalPorts are parallel canonicalizers over different proto field sets.
func canonicalParameters(params []*nfv1.ParameterSpec) []any {
	list := make([]any, 0, len(params))
	for _, p := range params {
		pm := map[string]any{}
		putStr(pm, jsonKeyName, p.GetName())
		putInt(pm, "type", int64(p.GetType()))
		putStr(pm, "default_value", p.GetDefaultValue())
		putStr(pm, "allowed_range", p.GetAllowedRange())
		putBool(pm, "required", p.GetRequired())
		putStr(pm, "cli_argument_mapping", p.GetCliArgumentMapping())
		putStr(pm, "mutually_exclusive_group", p.GetMutuallyExclusiveGroup())
		list = append(list, pm)
	}
	return list
}

func canonicalIntermediatePolicies(policies []*nfv1.IntermediateFilePolicy) []any {
	list := make([]any, 0, len(policies))
	for _, p := range policies {
		pm := map[string]any{}
		putStr(pm, "path_or_pattern", p.GetPathOrPattern())
		putInt(pm, "policy", int64(p.GetPolicy()))
		list = append(list, pm)
	}
	return list
}

func canonicalExecEnv(ee *nfv1.ExecutionEnvironmentSpec) map[string]any {
	m := map[string]any{}
	if ee == nil {
		return m
	}
	putStrList(m, "writable_paths", ee.GetWritablePaths())
	putStr(m, "network_policy", ee.GetNetworkPolicy())
	putBool(m, "requires_root", ee.GetRequiresRoot())
	putStrList(m, "required_capabilities", ee.GetRequiredCapabilities())
	return m
}

// ── presentation revision (digest-out, content-addressed) ─────────────────────

// buildPresentationRevision computes a content-addressed revision for the digest-out
// presentation. An absent or entirely-empty presentation yields no revision (empty id
// and nil record), so it never affects identity and never creates an empty revision.
func buildPresentationRevision(
	pres *nfv1.ToolFunctionPresentation, casHash string,
) (revisionID string, rev *index.ToolFunctionPresentationRevision) {
	canon := canonicalPresentation(pres)
	if len(canon) == 0 {
		return "", nil
	}
	b, err := json.Marshal(canon)
	if err != nil {
		b = []byte("nodevault.toolfunction.presentation_error\x00" + errValue(err))
	}
	sum := sha256.Sum256(b)
	revisionID = hex.EncodeToString(sum[:])
	return revisionID, &index.ToolFunctionPresentationRevision{
		RevisionID:       revisionID,
		CasHash:          casHash,
		PresentationJSON: string(b),
	}
}

func canonicalPresentation(p *nfv1.ToolFunctionPresentation) map[string]any {
	m := map[string]any{}
	if p == nil {
		return m
	}
	putStr(m, "label", p.GetLabel())
	putStr(m, "short_summary", p.GetShortSummary())
	putStr(m, "description", p.GetDescription())
	putStr(m, "category", p.GetCategory())
	putStrList(m, "tags", p.GetTags())
	putStr(m, "locale", p.GetLocale())
	opps := make([]any, 0, len(p.GetOutputPortPresentations()))
	for _, opp := range p.GetOutputPortPresentations() {
		om := map[string]any{}
		putStr(om, "port_name", opp.GetPortName())
		putStr(om, "downstream_compatibility_note", opp.GetDownstreamCompatibilityNote())
		opps = append(opps, om)
	}
	putList(m, "output_port_presentations", opps)
	return m
}

// ── canonical map/list builders (N1 omission) ─────────────────────────────────

func putStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func putInt(m map[string]any, k string, v int64) {
	if v != 0 {
		m[k] = v
	}
}

func putBool(m map[string]any, k string, v bool) {
	if v {
		m[k] = v
	}
}

func putStrList(m map[string]any, k string, v []string) {
	if len(v) == 0 {
		return
	}
	list := make([]any, len(v))
	for i := range v {
		list[i] = v[i]
	}
	m[k] = list
}

func putList(m map[string]any, k string, v []any) {
	if len(v) > 0 {
		m[k] = v
	}
}

func putMap(m map[string]any, k string, v map[string]any) {
	if len(v) > 0 {
		m[k] = v
	}
}
