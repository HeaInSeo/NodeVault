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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
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

// O-1 authoring binding bridge (CLOSED MINIMUM): a logical reference occupies one complete
// CommandContract.arguments element in an exact namespace, with the name grammar
// [A-Za-z][A-Za-z0-9._-]*. argumentReferenceLikeRE detects a reference attempt anywhere in an
// element (same word-token detector as the NodeKit client gate); an element that matches it
// but is not a whole-element reference is malformed or embedded and is rejected. Elements
// such as `{}`, `{foo}` or `{1.5}` stay literal (boundary not decided by the canon).
var (
	argumentReferenceRE     = regexp.MustCompile(`\A\{(param|input|output)\.([A-Za-z][A-Za-z0-9._-]*)\}\z`)
	argumentReferenceLikeRE = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*\.`)
)

// capabilityRE is the canonical Linux capability form required of a w2-set-v1
// required_capabilities entry (W2 FINAL: validated, never normalized).
var capabilityRE = regexp.MustCompile(`\ACAP_[A-Z][A-Z0-9_]*\z`)

// RegisterToolFunction validates a typed ToolFunctionSpec declaration, computes the
// NodeVault-owned tool_function_digest and cas_hash over its own canonical JSON,
// durably registers the runnable record (+ optional presentation revision + operation
// receipt) atomically, and returns the identity. The canonicalizer is selected per
// operation (issue #19 DC-R1-NV-C1): a new request_id must name w2-set-v1, while a replay
// is compared under the version and full request basis stored in its receipt. It is
// idempotent by request_id and by provable content (cas_hash); a new successful runnable
// record starts lifecycle Active and re-registration never resurrects a Retracted/Deleted
// record.
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
	version := req.GetCanonicalizationVersion()
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
	// The presentation and the digest-out envelope get the same gate for the request basis.
	if err := rejectUnknownRequestFields(req); err != nil {
		return nil, err
	}
	// Select the canonicalizer from the durable receipt BEFORE any canonicalization: an
	// existing operation is never re-read through the new v1 canonicalizer.
	if err := s.checkCanonicalizationSelection(req.GetRequestId(), version); err != nil {
		return nil, err
	}
	if err := validateToolFunctionSpec(req.GetSpec(), version); err != nil {
		return nil, err
	}
	if err := validateToolFunctionPresentation(req.GetPresentation(), req.GetSpec()); err != nil {
		return nil, err
	}
	if err := validateToolFunctionValidationPolicy(req.GetValidationPolicy(), req.GetSpec()); err != nil {
		return nil, err
	}

	// base_tool_spec_digest is both an identity preimage and a typed lineage/dependency
	// reference (W1 contract). A new immutable ToolFunction must not permanently record a
	// dangling base: the digest must resolve to an existing authoritative ResolvedToolSpec.
	// Look up the normalized digest and fail closed with NotFound BEFORE computing or
	// persisting any identity (zero mutation). (Scope is strictly existence — no lifecycle,
	// tag/latest resolution, or eligibility re-evaluation.)
	baseSpec, err := s.store.GetResolvedToolSpecByDigest(baseToolSpecDigest)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound,
				"base_tool_spec_digest %q does not resolve to a registered ResolvedToolSpec", baseToolSpecDigest)
		}
		return nil, status.Errorf(codes.Internal, "resolve base tool spec: %v", err)
	}
	// The base ResolvedToolSpec must be interpretable under the frozen schema/derivation
	// provenance (W3-PRE) before its lineage is recorded into a new immutable ToolFunction:
	// a half-populated or unknown provenance pair means NodeVault can no longer vouch for how
	// that base was derived, so fail closed rather than persisting derived lineage against it
	// (mirrors the SubmitToolBuild / ResolveToolSpec read-back guards — same shared record,
	// same fail-closed contract at every consume site).
	if _, provErr := resolve.EffectiveProvenance(
		baseSpec.RawSpecSchemaVersion, baseSpec.DerivationVersion); provErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "base tool spec provenance: %v", provErr)
	}

	// NodeVault-owned identity (N3): canonical JSON + SHA256, over frozen preimages, plus the
	// full request basis/fingerprint the operation receipt records. Encoding failures are
	// errors, never a sentinel identity.
	d, err := deriveToolFunction(version, baseToolSpecDigest, imageDigest, req)
	if err != nil {
		return nil, err
	}

	rec := index.RegisteredToolFunction{
		CasHash:                 d.casHash,
		ToolFunctionDigest:      d.toolFunctionDigest,
		FunctionImageDigest:     imageDigest,
		BaseToolSpecDigest:      baseToolSpecDigest,
		ArtifactKind:            index.KindToolFunction,
		PresentationRevisionID:  d.presentationRevID,
		RequestID:               req.GetRequestId(),
		CanonicalizationVersion: version,
		LifecyclePhase:          index.PhaseActive,
		IntegrityHealth:         index.HealthPartial, // Partial until reconcile observes Harbor
	}

	stored, _, err := s.store.RegisterToolFunctionAtomic(d.op, rec, d.presentationRev)
	if err != nil {
		return nil, toolFunctionStoreError(req.GetRequestId(), err)
	}

	return &nfv1.RegisterToolFunctionResponse{
		ToolFunctionDigest:      stored.ToolFunctionDigest,
		PresentationRevisionId:  stored.PresentationRevisionID,
		CasHash:                 stored.CasHash,
		CanonicalizationVersion: version,
	}, nil
}

// checkCanonicalizationSelection applies the DC-R1-NV-C1 version selection: an unsupported
// version is rejected fail-closed, then the durable receipt of request_id is looked up (a
// lookup failure is never treated as absence). A replay must carry the receipt's version
// (else conflict) and an UNKNOWN_LEGACY receipt cannot be replayed; a new request_id must
// explicitly name w2-set-v1 — no default is assumed and legacy-order-v0 is replay-only.
func (s *ToolRegistryService) checkCanonicalizationSelection(requestID, version string) error {
	switch version {
	case "", index.CanonicalizationW2SetV1, index.CanonicalizationLegacyOrderV0:
	default:
		return status.Errorf(codes.InvalidArgument, "unsupported canonicalization_version %q", version)
	}
	prior, err := s.store.GetToolFunctionRequestRecord(requestID)
	switch {
	case err == nil:
		if !prior.BasisKnown() {
			return status.Errorf(codes.FailedPrecondition,
				"request_id %q has an UNKNOWN_LEGACY receipt (no canonicalizer version / full request basis); "+
					"replay equality cannot be proven", requestID)
		}
		if prior.CanonicalizationVersion != version {
			return status.Errorf(codes.AlreadyExists,
				"request_id %q was already used with canonicalization_version %q", requestID, prior.CanonicalizationVersion)
		}
		return nil
	case errors.Is(err, index.ErrNotFound):
		switch version {
		case index.CanonicalizationW2SetV1:
			return nil
		case "":
			return status.Errorf(codes.InvalidArgument,
				"canonicalization_version is required for a new request_id (use %q; no default is assumed)",
				index.CanonicalizationW2SetV1)
		default:
			return status.Errorf(codes.FailedPrecondition,
				"canonicalization_version %q is replay-only; new registrations must use %q",
				version, index.CanonicalizationW2SetV1)
		}
	default:
		return status.Errorf(codes.Internal, "look up request receipt: %v", err)
	}
}

// toolFunctionStoreError maps a RegisterToolFunctionAtomic failure to its gRPC status.
func toolFunctionStoreError(requestID string, err error) error {
	switch {
	case errors.Is(err, index.ErrToolFunctionRequestConflict):
		return status.Errorf(codes.AlreadyExists,
			"request_id %q was already used for different content", requestID)
	case errors.Is(err, index.ErrToolFunctionEnvelopeConflict):
		// W2-OUTSIDE-DIGEST-REREG-01: a provable validation_policy/environment_hints mismatch for
		// an existing tool_function_digest is an explicit conflict. An unprovable (UNKNOWN_LEGACY)
		// envelope stays FailedPrecondition below.
		return status.Errorf(codes.AlreadyExists, "register tool function: %v", err)
	case errors.Is(err, index.ErrToolFunctionRequestUnknownLegacy),
		errors.Is(err, index.ErrToolFunctionIdentityAmbiguous),
		errors.Is(err, index.ErrToolFunctionWriterFenced):
		return status.Errorf(codes.FailedPrecondition, "register tool function: %v", err)
	default:
		return status.Errorf(codes.Internal, "register tool function: %v", err)
	}
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

// rejectUnknownPresentationFields fails closed if the presentation, or any message nested
// within it, carries unknown protobuf fields (a newer client). The presentation is persisted
// as a content-addressed revision (canonicalPresentation serializes only known accessors), so
// an unknown field would be silently dropped — the revision id/JSON would not reflect it, and
// an unknown-only presentation would even collapse to an empty revision. Reject before the
// revision is hashed/persisted, mirroring the spec unknown-field gate.
func rejectUnknownPresentationFields(pres *nfv1.ToolFunctionPresentation) error {
	if pres == nil {
		return nil
	}
	if name, found := firstUnknownField(pres.ProtoReflect()); found {
		return status.Errorf(codes.InvalidArgument,
			"presentation contains unknown protobuf field(s) in %s; revision would be lossy", name)
	}
	return nil
}

// rejectUnknownRequestFields applies the unknown-field gates to every request part that enters
// an identity or the receipt's request basis: spec, presentation, and the digest-out envelope.
func rejectUnknownRequestFields(req *nfv1.RegisterToolFunctionRequest) error {
	if err := rejectUnknownSpecFields(req.GetSpec()); err != nil {
		return err
	}
	if err := rejectUnknownPresentationFields(req.GetPresentation()); err != nil {
		return err
	}
	return rejectUnknownEnvelopeFields(req)
}

// rejectUnknownEnvelopeFields fails closed if validation_policy or environment_hints carries
// unknown protobuf fields or an undefined ObservationLevel. Both are digest-out, but they belong
// to the full request basis a receipt is compared on (DC-R1-NV-C1): a field the canonicalizer
// cannot see would make two different requests compare equal.
func rejectUnknownEnvelopeFields(req *nfv1.RegisterToolFunctionRequest) error {
	if vp := req.GetValidationPolicy(); vp != nil {
		if name, found := firstUnknownField(vp.ProtoReflect()); found {
			return status.Errorf(codes.InvalidArgument,
				"validation_policy contains unknown protobuf field(s) in %s; request basis would be incomplete", name)
		}
		level := vp.GetValidationRequirements().GetMinimumObservationLevel()
		if _, ok := nfv1.ObservationLevel_name[int32(level)]; !ok {
			return status.Errorf(codes.InvalidArgument, "unknown minimum_observation_level %d", int32(level))
		}
	}
	if eh := req.GetEnvironmentHints(); eh != nil {
		if name, found := firstUnknownField(eh.ProtoReflect()); found {
			return status.Errorf(codes.InvalidArgument,
				"environment_hints contains unknown protobuf field(s) in %s; request basis would be incomplete", name)
		}
	}
	return nil
}

// validateToolFunctionSpec validates the spec under the operation's canonicalizer version.
// w2-set-v1 additionally enforces the W2 REPEATED-FIELD ORDERING FINAL entry rules (no empty
// set-like entry, canonical CAP_* capabilities, unique intermediate pattern) and rejects every
// exact duplicate in the nine set-like fields; legacy-order-v0 (replay-only) keeps the original
// checks so a provable legacy replay is compared exactly as it was accepted.
func validateToolFunctionSpec(spec *nfv1.ToolFunctionSpec, version string) error {
	if err := validateToolFunctionSpecCommon(spec); err != nil {
		return err
	}
	if version == index.CanonicalizationLegacyOrderV0 {
		return nil
	}
	if err := validateSetLikeEntries(spec); err != nil {
		return err
	}
	// Exact duplicates are detected on the canonical element bytes the v1 canonicalizer sorts by.
	_, err := canonicalToolFunctionSpecV1(spec)
	return err
}

func validateToolFunctionSpecCommon(spec *nfv1.ToolFunctionSpec) error {
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
	if err := validateArgumentReferences(spec); err != nil {
		return err
	}
	return validateIntermediateFilePolicyKinds(spec.GetIntermediateFilePolicies())
}

// validateSetLikeEntries rejects invalid repeated entries in the set-like fields (w2-set-v1):
// semantic-empty omission applies only to singular messages, so an empty environment name,
// companion file, intermediate pattern or writable path is rejected rather than erased, a
// required capability must already be in canonical CAP_* form (validated, never rewritten),
// and two intermediate policies may not share a pattern.
func validateSetLikeEntries(spec *nfv1.ToolFunctionSpec) error {
	for _, e := range spec.GetCommand().GetEnvironment() {
		if strings.TrimSpace(e.GetName()) == "" {
			return status.Error(codes.InvalidArgument, "command.environment entry name must not be empty")
		}
	}
	for _, ports := range [][]*nfv1.FunctionPortSpec{spec.GetInputs(), spec.GetOutputs()} {
		for _, p := range ports {
			for _, f := range p.GetCompanionFiles() {
				if strings.TrimSpace(f) == "" {
					return status.Errorf(codes.InvalidArgument, "port %q companion_files entry must not be empty", p.GetName())
				}
			}
		}
	}
	patterns := make(map[string]struct{}, len(spec.GetIntermediateFilePolicies()))
	for _, p := range spec.GetIntermediateFilePolicies() {
		pattern := p.GetPathOrPattern()
		if strings.TrimSpace(pattern) == "" {
			return status.Error(codes.InvalidArgument, "intermediate_file_policies path_or_pattern must not be empty")
		}
		if _, dup := patterns[pattern]; dup {
			return status.Errorf(codes.InvalidArgument, "duplicate intermediate_file_policies pattern %q", pattern)
		}
		patterns[pattern] = struct{}{}
	}
	ee := spec.GetExecutionEnvironment()
	for _, w := range ee.GetWritablePaths() {
		if strings.TrimSpace(w) == "" {
			return status.Error(codes.InvalidArgument, "execution_environment.writable_paths entry must not be empty")
		}
	}
	for _, c := range ee.GetRequiredCapabilities() {
		if !capabilityRE.MatchString(c) {
			return status.Errorf(codes.InvalidArgument,
				"execution_environment.required_capabilities entry %q is not a canonical CAP_* capability", c)
		}
	}
	return nil
}

// validateArgumentReferences enforces the O-1 authoring binding bridge on
// CommandContract.arguments before any digest or persistent mutation:
//   - a reference is a whole element `{param|input|output.<name>}`; malformed or embedded
//     reference attempts are rejected;
//   - `{param.<name>}` must resolve to exactly one declared parameter (names are already
//     unique); repeated references are allowed;
//   - `{input.*}` / `{output.*}` are valid authoring intent but not runnable until an approved
//     Runtime Invocation Finalization (B) profile exists, so registration rejects them;
//   - every declared parameter must be consumed by at least one reference
//     (cli_argument_mapping renders a referenced parameter; it never inserts one).
//
// Only new registrations and replays are affected: stored records keep no spec and are never
// re-validated or rehashed.
func validateArgumentReferences(spec *nfv1.ToolFunctionSpec) error {
	declared := make(map[string]struct{}, len(spec.GetParameters()))
	for _, p := range spec.GetParameters() {
		declared[p.GetName()] = struct{}{}
	}
	consumed := make(map[string]struct{}, len(declared))
	for i, arg := range spec.GetCommand().GetArguments() {
		m := argumentReferenceRE.FindStringSubmatch(arg)
		if m == nil {
			if argumentReferenceLikeRE.MatchString(arg) {
				return status.Errorf(codes.InvalidArgument,
					"command argument %d %q is a malformed or embedded reference; a reference must be a whole "+
						"element {param|input|output.<name>}", i, arg)
			}
			continue // literal element
		}
		namespace, name := m[1], m[2]
		if namespace != "param" {
			return status.Errorf(codes.InvalidArgument,
				"command argument %d %q: direct {%s.*} references are not runnable before an approved "+
					"runtime invocation finalization profile", i, arg, namespace)
		}
		if _, ok := declared[name]; !ok {
			return status.Errorf(codes.InvalidArgument,
				"command argument %d %q references undeclared parameter %q", i, arg, name)
		}
		consumed[name] = struct{}{}
	}
	for _, p := range spec.GetParameters() {
		if _, ok := consumed[p.GetName()]; !ok {
			return status.Errorf(codes.InvalidArgument,
				"parameter %q is declared but not referenced by any {param.%s} command argument", p.GetName(), p.GetName())
		}
	}
	return nil
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

// toolFunctionDerivation is everything RegisterToolFunction derives from one request under
// the operation's canonicalizer: the identities, the presentation revision, and the receipt's
// full request basis/fingerprint.
type toolFunctionDerivation struct {
	toolFunctionDigest string
	casHash            string
	presentationRevID  string
	presentationRev    *index.ToolFunctionPresentationRevision
	op                 index.ToolFunctionOperation
}

// deriveToolFunction computes the identities and the full request basis under version.
// tool_function_digest / cas_hash formulas and preimage membership are unchanged; the version
// is derivation provenance recorded in the receipt, never a new preimage member. The basis is
// the canonical full request — version, normalized base/image digests, canonical spec and
// presentation, validation_policy and environment_hints (request_id is the lookup key, not
// basis) — and the fingerprint is its SHA256.
func deriveToolFunction(
	version, baseToolSpecDigest, imageDigest string, req *nfv1.RegisterToolFunctionRequest,
) (*toolFunctionDerivation, error) {
	canonSpec, err := canonicalToolFunctionSpecFor(version, req.GetSpec())
	if err != nil {
		return nil, err
	}
	d := &toolFunctionDerivation{}
	if d.toolFunctionDigest, err = canonicalSHA256(map[string]any{
		"base_tool_spec_digest": baseToolSpecDigest,
		"spec":                  canonSpec,
	}); err != nil {
		return nil, err
	}
	if d.casHash, err = computeToolFunctionCasHash(d.toolFunctionDigest, imageDigest); err != nil {
		return nil, err
	}
	// Presentation is digest-out; persist it as a content-addressed revision.
	d.presentationRevID, d.presentationRev, err = buildPresentationRevision(req.GetPresentation(), d.casHash)
	if err != nil {
		return nil, err
	}

	basis := map[string]any{
		"canonicalization_version": version,
		"base_tool_spec_digest":    baseToolSpecDigest,
		"image_digest":             imageDigest,
		"spec":                     canonSpec,
	}
	if pres := canonicalPresentation(req.GetPresentation()); len(pres) > 0 {
		basis["presentation"] = pres
	}
	vp := req.GetValidationPolicy()
	putMsg(basis, "validation_policy", vp != nil, canonicalValidationPolicy(vp))
	eh := req.GetEnvironmentHints()
	putMsg(basis, "environment_hints", eh != nil, canonicalEnvironmentHints(eh))
	basisJSON, err := json.Marshal(basis)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "canonical encoding of request basis failed: %v", err)
	}
	sum := sha256.Sum256(basisJSON)
	d.op = index.ToolFunctionOperation{
		RequestID:               req.GetRequestId(),
		CanonicalizationVersion: version,
		RequestFingerprint:      hex.EncodeToString(sum[:]),
		RequestBasisJSON:        string(basisJSON),
	}
	return d, nil
}

func computeToolFunctionCasHash(toolFunctionDigest, functionImageDigest string) (string, error) {
	return canonicalSHA256(map[string]any{
		"tool_function_digest":  toolFunctionDigest,
		"function_image_digest": functionImageDigest,
	})
}

// canonicalSHA256 marshals v to JSON (encoding/json sorts object keys, giving one
// canonical byte form — N2) and returns the lowercase hex SHA256, matching the bare-
// hex casHash convention used by catalog.SaveWithCasHash. A canonical encoding error is
// returned fail-closed: no sentinel is ever hashed into an identity (DC-R1-NV-C1).
func canonicalSHA256(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", status.Errorf(codes.Internal, "canonical encoding failed: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalToolFunctionSpecFor renders the spec under the operation's canonicalizer.
func canonicalToolFunctionSpecFor(version string, spec *nfv1.ToolFunctionSpec) (map[string]any, error) {
	if version == index.CanonicalizationLegacyOrderV0 {
		return canonicalToolFunctionSpec(spec), nil
	}
	return canonicalToolFunctionSpecV1(spec)
}

// canonicalToolFunctionSpecV1 is the w2-set-v1 canonicalizer (W2 REPEATED-FIELD ORDERING
// FINAL): it is the legacy tree with every set-like field — environment, inputs, outputs,
// parameters, success_exit_codes, companion_files, intermediate_file_policies,
// writable_paths, required_capabilities — put in deterministic total order, and any exact
// duplicate rejected InvalidArgument. command.arguments alone keeps its authored order.
func canonicalToolFunctionSpecV1(spec *nfv1.ToolFunctionSpec) (map[string]any, error) {
	m := canonicalToolFunctionSpec(spec)
	if cm, ok := m["command"].(map[string]any); ok {
		if err := sortCanonicalSetIn(cm, "environment", "command.environment"); err != nil {
			return nil, err
		}
		if err := sortCanonicalSetIn(cm, "success_exit_codes", "command.success_exit_codes"); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"inputs", "outputs"} {
		ports, _ := m[field].([]any)
		for _, p := range ports {
			if pm, ok := p.(map[string]any); ok {
				if err := sortCanonicalSetIn(pm, "companion_files", field+".companion_files"); err != nil {
					return nil, err
				}
			}
		}
		if err := sortCanonicalSetIn(m, field, field); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"parameters", "intermediate_file_policies"} {
		if err := sortCanonicalSetIn(m, field, field); err != nil {
			return nil, err
		}
	}
	if em, ok := m["execution_environment"].(map[string]any); ok {
		if err := sortCanonicalSetIn(em, "writable_paths", "execution_environment.writable_paths"); err != nil {
			return nil, err
		}
		if err := sortCanonicalSetIn(em, "required_capabilities", "execution_environment.required_capabilities"); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// sortCanonicalSetIn replaces m[key] (a canonical list, if present) with its elements sorted
// by their canonical JSON bytes, rejecting an exact duplicate element InvalidArgument.
func sortCanonicalSetIn(m map[string]any, key, field string) error {
	list, ok := m[key].([]any)
	if !ok {
		return nil
	}
	type keyed struct {
		enc []byte
		v   any
	}
	elems := make([]keyed, 0, len(list))
	for _, v := range list {
		enc, err := json.Marshal(v)
		if err != nil {
			return status.Errorf(codes.Internal, "canonical encoding of %s failed: %v", field, err)
		}
		elems = append(elems, keyed{enc: enc, v: v})
	}
	sort.Slice(elems, func(i, j int) bool { return bytes.Compare(elems[i].enc, elems[j].enc) < 0 })
	sorted := make([]any, len(elems))
	for i := range elems {
		if i > 0 && bytes.Equal(elems[i-1].enc, elems[i].enc) {
			return status.Errorf(codes.InvalidArgument, "duplicate %s entry %s", field, elems[i].enc)
		}
		sorted[i] = elems[i].v
	}
	m[key] = sorted
	return nil
}

// canonicalToolFunctionSpec is the legacy-order-v0 canonicalizer (replay-only) and the base
// tree w2-set-v1 sorts: unspecified/empty fields are omitted (N1), repeated fields keep their
// authored order, and enums are emitted as their integer value only when non-zero
// (CARDINALITY_UNSPECIFIED etc. are omitted, never rewritten to SINGLE).
func canonicalToolFunctionSpec(spec *nfv1.ToolFunctionSpec) map[string]any {
	m := map[string]any{}
	if spec == nil {
		return m
	}
	// Singular nested messages use putMsg so a PRESENT-but-empty message (e.g. command: {})
	// is distinguished from an absent one in the digest (N1): an empty-but-present message is
	// included as {}, an absent message is omitted.
	putMsg(m, "command", spec.GetCommand() != nil, canonicalCommand(spec.GetCommand()))
	putList(m, "inputs", canonicalPorts(spec.GetInputs()))
	putList(m, "outputs", canonicalPorts(spec.GetOutputs()))
	putList(m, "parameters", canonicalParameters(spec.GetParameters()))
	putList(m, "intermediate_file_policies", canonicalIntermediatePolicies(spec.GetIntermediateFilePolicies()))
	ee := spec.GetExecutionEnvironment()
	putMsg(m, "execution_environment", ee != nil, canonicalExecEnv(ee))
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
		// Present (tp != nil): include even if both seconds are zero, to distinguish a
		// present-but-empty timeout_policy from an absent one (N1).
		putMsg(m, "timeout_policy", true, tm)
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
) (revisionID string, rev *index.ToolFunctionPresentationRevision, err error) {
	canon := canonicalPresentation(pres)
	if len(canon) == 0 {
		return "", nil, nil
	}
	b, err := json.Marshal(canon)
	if err != nil {
		return "", nil, status.Errorf(codes.Internal, "canonical encoding of presentation failed: %v", err)
	}
	sum := sha256.Sum256(b)
	revisionID = hex.EncodeToString(sum[:])
	return revisionID, &index.ToolFunctionPresentationRevision{
		RevisionID:       revisionID,
		CasHash:          casHash,
		PresentationJSON: string(b),
	}, nil
}

// canonicalValidationPolicy renders the digest-out validation policy for the request basis
// with the same N1 rules; its repeated values keep their authored order (no set-like rule is
// closed for them) and the required_coverage map is key-ordered by the encoder.
func canonicalValidationPolicy(vp *nfv1.ToolFunctionValidationPolicy) map[string]any {
	m := map[string]any{}
	if vp == nil {
		return m
	}
	fixtures := make([]any, 0, len(vp.GetFixtureReferences()))
	for _, f := range vp.GetFixtureReferences() {
		fm := map[string]any{}
		putStr(fm, "local_path", f.GetLocalPath())
		putStr(fm, "content_digest", f.GetContentDigest())
		fixtures = append(fixtures, fm)
	}
	putList(m, "fixture_references", fixtures)
	results := make([]any, 0, len(vp.GetExpectedResults()))
	for _, r := range vp.GetExpectedResults() {
		rm := map[string]any{}
		putStr(rm, "output_port_name", r.GetOutputPortName())
		putStr(rm, "expected_value_or_rule", r.GetExpectedValueOrRule())
		results = append(results, rm)
	}
	putList(m, "expected_results", results)
	if vr := vp.GetValidationRequirements(); vr != nil {
		rm := map[string]any{}
		putInt(rm, "minimum_observation_level", int64(vr.GetMinimumObservationLevel()))
		if cov := vr.GetRequiredCoverage(); len(cov) > 0 {
			cm := make(map[string]any, len(cov))
			for k, v := range cov {
				cm[k] = v
			}
			rm["required_coverage"] = cm
		}
		putMsg(m, "validation_requirements", true, rm)
	}
	return m
}

// canonicalEnvironmentHints renders the digest-out environment hints for the request basis.
func canonicalEnvironmentHints(eh *nfv1.ToolFunctionEnvironmentHints) map[string]any {
	m := map[string]any{}
	if eh == nil {
		return m
	}
	putStrList(m, "supported_platforms", eh.GetSupportedPlatforms())
	if rc := eh.GetEnforcedResources(); rc != nil {
		rm := map[string]any{}
		putStr(rm, "cpu_request", rc.GetCpuRequest())
		putStr(rm, "cpu_limit", rc.GetCpuLimit())
		putStr(rm, "memory_request", rc.GetMemoryRequest())
		putStr(rm, "memory_limit", rc.GetMemoryLimit())
		putStr(rm, "storage_request", rc.GetStorageRequest())
		putStr(rm, "storage_limit", rc.GetStorageLimit())
		putInt(rm, "max_execution_time_seconds", int64(rc.GetMaxExecutionTimeSeconds()))
		putInt(rm, "parallelism", int64(rc.GetParallelism()))
		putMsg(m, "enforced_resources", true, rm)
	}
	return m
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

// putMsg includes a singular nested message's canonical map when the message is PRESENT on
// the wire, even if it canonicalizes to an empty object — so an explicitly-empty message
// (e.g. command: {}) is distinguished from an absent one in the digest (N1). An absent
// message (present=false) is omitted. Repeated messages do not use this: proto3 has no
// presence for a repeated field, so present-but-empty and absent are indistinguishable and
// both correctly omit.
func putMsg(m map[string]any, k string, present bool, v map[string]any) {
	if present {
		m[k] = v
	}
}
