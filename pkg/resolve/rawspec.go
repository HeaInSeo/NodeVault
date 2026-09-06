package resolve

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// W3-PRE: NodeVault-owned raw_spec schema authority + frozen derivation.
//
// The raw_spec wire contract was historically an accidental encoding/json -> protobuf
// BuildRequest shape. This file makes it an explicit, NodeVault-owned schema FAMILY with a
// strict typed parser, while preserving legacy first-image (v0) behavior and digests
// byte-for-byte. It defines schema/derivation provenance so a future parser change cannot
// silently reinterpret historical resolved records.
//
// It does NOT enable kind=2 (BUILD_KIND_TOOLFUNCTIONSPEC) build execution — the v1 raw_spec
// is parsed, validated, and identity-derived for schema authority only.

// Machine-checkable JSON Schema governance artifacts (draft 2020-12). They are the
// authoritative, externally-checkable definition of each raw_spec shape; the strict typed
// parsers below are the runtime enforcement and MUST agree with them.
//
//go:embed schemas/legacy_v0.schema.json
var legacyV0SchemaJSON []byte

//go:embed schemas/nodevault.build.raw_spec.v1.schema.json
var buildRawSpecV1SchemaJSON []byte

const (
	// SchemaLegacyV0 identifies the historical first-image ToolSpec raw_spec shape. It has
	// no self-identifying field; it is the schema of any raw_spec that is not v1.
	SchemaLegacyV0 = "nodevault.toolspec.raw_spec.v0"
	// SchemaBuildV1 identifies the second-image ToolFunction build raw_spec (kind=2). It is
	// self-identifying via its schema_version field, whose value equals this constant.
	SchemaBuildV1 = "nodevault.build.raw_spec.v1"

	// DerivationV1 is the current resolver/parser derivation identity. It is frozen onto each
	// resolved record so a later derivation change cannot silently reinterpret historical
	// records; an unknown/newer derivation on read fails closed rather than falling back.
	DerivationV1 = "nodevault.resolve.v1"

	// buildKindToolFunctionSpec is the kind value of a v1 build raw_spec (mirrors the
	// nfv1.BuildKind_BUILD_KIND_TOOLFUNCTIONSPEC enum value; not a public-proto dependency).
	buildKindToolFunctionSpec = 2
)

// Provenance is the frozen schema/derivation identity of a resolved raw_spec.
type Provenance struct {
	// SchemaVersion is the raw_spec schema id the record was resolved under
	// (SchemaLegacyV0 or SchemaBuildV1).
	SchemaVersion string
	// DerivationVersion is the resolver derivation identity (DerivationV1).
	DerivationVersion string
}

// LegacyV0SchemaJSON / BuildRawSpecV1SchemaJSON expose the embedded schema artifacts (for
// documentation, external validation, and agreement tests).
func LegacyV0SchemaJSON() []byte       { return legacyV0SchemaJSON }
func BuildRawSpecV1SchemaJSON() []byte { return buildRawSpecV1SchemaJSON }

// RawSpecV1 is the strict typed DTO for a nodevault.build.raw_spec.v1 document. It is the
// internal representation; a v1 raw_spec is never decoded directly into the public
// BuildRequest as the contract authority.
type RawSpecV1 struct {
	SchemaVersion   string `json:"schema_version"`
	Kind            int    `json:"kind"`
	BaseImageDigest string `json:"base_image_digest"`
	Script          string `json:"script"`
}

// IsV1RawSpec reports whether rawSpec is a v1-schema CANDIDATE: a JSON object that carries a
// schema_version field at all (any value). Detection is by field PRESENCE, not value, so a
// document with a wrong/unknown schema_version is routed to strict v1 parsing and rejected
// (fail-closed, no legacy fallback) rather than silently mis-resolved as legacy. Legacy
// first-image raw_specs never carry schema_version, so they are unaffected and resolve
// byte-for-byte as before.
func IsV1RawSpec(rawSpec string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawSpec)), &probe); err != nil {
		return false
	}
	_, present := probe["schema_version"]
	return present
}

// DetectSchema returns the schema id a raw_spec resolves under.
func DetectSchema(rawSpec string) string {
	if IsV1RawSpec(rawSpec) {
		return SchemaBuildV1
	}
	return SchemaLegacyV0
}

// ParseRawSpecV1 strictly parses and validates a v1 build raw_spec, returning the normalized
// DTO (base image digest hex lowercased). It rejects unknown fields, trailing content, a
// wrong/absent schema_version, a wrong kind, a malformed base image digest, and an empty
// script — fail-closed, matching nodevault.build.raw_spec.v1.schema.json exactly.
func ParseRawSpecV1(rawSpec string) (RawSpecV1, error) {
	dec := json.NewDecoder(strings.NewReader(rawSpec))
	dec.DisallowUnknownFields()
	var v RawSpecV1
	if err := dec.Decode(&v); err != nil {
		return RawSpecV1{}, fmt.Errorf("v1 raw_spec parse: %w", err)
	}
	// Reject trailing content after the single JSON document.
	if _, err := dec.Token(); err != io.EOF {
		return RawSpecV1{}, fmt.Errorf("v1 raw_spec has trailing content after the document")
	}
	if v.SchemaVersion != SchemaBuildV1 {
		return RawSpecV1{}, fmt.Errorf("v1 raw_spec schema_version must be %q, got %q", SchemaBuildV1, v.SchemaVersion)
	}
	if v.Kind != buildKindToolFunctionSpec {
		return RawSpecV1{}, fmt.Errorf(
			"v1 raw_spec kind must be %d (BUILD_KIND_TOOLFUNCTIONSPEC), got %d", buildKindToolFunctionSpec, v.Kind)
	}
	if !IsSHA256Digest(v.BaseImageDigest) {
		return RawSpecV1{}, fmt.Errorf("v1 raw_spec base_image_digest must match sha256:<64 hex chars>")
	}
	if strings.TrimSpace(v.Script) == "" {
		return RawSpecV1{}, fmt.Errorf("v1 raw_spec script must not be empty")
	}
	// Normalize the accepted digest hex to canonical lowercase before identity derivation so
	// upper/lower spellings of the same digest converge to one v1 identity.
	v.BaseImageDigest = strings.ToLower(strings.TrimSpace(v.BaseImageDigest))
	return v, nil
}

// canonicalV1 renders the normalized v1 DTO as deterministic canonical JSON (struct field
// order is fixed, so input key order/whitespace do not affect it, and the lowercased base
// digest makes hex-case-variant inputs converge).
func canonicalV1(v RawSpecV1) string {
	b, err := json.Marshal(v)
	if err != nil {
		// The DTO is plain scalars and cannot fail to marshal; fall back to a non-colliding
		// sentinel over the error rather than silently hashing "".
		return "nodevault.build.raw_spec.v1.canonicalization_error\x00" + err.Error()
	}
	return string(b)
}

// ResolveRawSpec is the schema-aware resolution entry point. It detects the raw_spec schema,
// resolves it, and returns the deterministic digests plus the frozen provenance.
//   - legacy-v0: delegates to Resolve unchanged (byte-for-byte identical digests).
//   - v1: strictly parses/validates, derives the recipe digest from the NORMALIZED v1 content
//     (not canonicalSpec of the raw text) and the base image digest from the v1 field (never
//     legacy image_uri extraction), then reuses the shared build-plan/tool-spec digest chain.
func ResolveRawSpec(req Request, ctx Context) (Resolved, Provenance, error) {
	if !IsV1RawSpec(req.RawSpec) {
		resolved, err := Resolve(req, ctx)
		if err != nil {
			return Resolved{}, Provenance{}, err
		}
		return resolved, Provenance{SchemaVersion: SchemaLegacyV0, DerivationVersion: DerivationV1}, nil
	}

	v, err := ParseRawSpecV1(req.RawSpec)
	if err != nil {
		return Resolved{}, Provenance{}, err
	}
	recipeDigest := sha256Hex([]byte("nodevault.recipe_inputs.v1\n" + canonicalV1(v)))

	ctx.BaseImageDigest = v.BaseImageDigest // v1 derives the base digest from its own field
	buildPlanDigest, err := BuildPlanDigest(recipeDigest, ctx)
	if err != nil {
		return Resolved{}, Provenance{}, err
	}
	builderIdentity := strings.TrimSpace(ctx.BuilderIdentity)
	if builderIdentity == "" {
		builderIdentity = defaultBuilderIdentity
	}
	payload, err := json.Marshal(struct {
		ToolName           string `json:"tool_name"`
		Version            string `json:"version,omitempty"`
		RecipeInputsDigest string `json:"recipe_inputs_digest"`
		BuildPlanDigest    string `json:"build_plan_digest"`
		BaseImageDigest    string `json:"base_image_digest,omitempty"`
	}{
		ToolName:           strings.TrimSpace(req.ToolName),
		Version:            strings.TrimSpace(req.Version),
		RecipeInputsDigest: recipeDigest,
		BuildPlanDigest:    buildPlanDigest,
		BaseImageDigest:    v.BaseImageDigest,
	})
	if err != nil {
		return Resolved{}, Provenance{}, fmt.Errorf("marshal v1 tool spec digest payload: %w", err)
	}
	resolved := Resolved{
		ToolSpecDigest:     sha256Hex(append([]byte("nodevault.tool_spec.v1\n"), payload...)),
		RecipeInputsDigest: recipeDigest,
		BuildPlanDigest:    buildPlanDigest,
		BuilderIdentity:    builderIdentity,
		BaseImageRef:       "",
		BaseImageDigest:    v.BaseImageDigest,
	}
	return resolved, Provenance{SchemaVersion: SchemaBuildV1, DerivationVersion: DerivationV1}, nil
}

// EffectiveProvenance maps a resolved record's stored provenance to its effective identity,
// failing closed. Records written before W3-PRE have no stored provenance: they map ONLY to
// the historical legacy-v0 schema + resolve-v1 derivation (no latest-parser fallback). A
// stored but UNKNOWN schema/derivation (e.g. a newer writer) fails closed rather than being
// reinterpreted under the current parser.
func EffectiveProvenance(storedSchemaVersion, storedDerivationVersion string) (Provenance, error) {
	schema := strings.TrimSpace(storedSchemaVersion)
	derivation := strings.TrimSpace(storedDerivationVersion)
	if schema == "" && derivation == "" {
		// pre-W3-PRE record: historical legacy-v0 + resolve-v1 only.
		return Provenance{SchemaVersion: SchemaLegacyV0, DerivationVersion: DerivationV1}, nil
	}
	if schema == "" {
		schema = SchemaLegacyV0
	}
	if schema != SchemaLegacyV0 && schema != SchemaBuildV1 {
		return Provenance{}, fmt.Errorf("unknown raw_spec schema version %q; refusing to reinterpret (fail-closed)", schema)
	}
	if derivation != DerivationV1 {
		return Provenance{}, fmt.Errorf(
			"unknown raw_spec derivation version %q; refusing to reinterpret (fail-closed)", derivation)
	}
	return Provenance{SchemaVersion: schema, DerivationVersion: derivation}, nil
}

// schemasAgree is a lightweight sanity hook used by tests to assert the embedded schema
// artifacts are well-formed JSON (machine-checkable) and carry the expected $id.
func schemasAgree() error {
	for _, s := range [][]byte{legacyV0SchemaJSON, buildRawSpecV1SchemaJSON} {
		var doc map[string]any
		if err := json.Unmarshal(s, &doc); err != nil {
			return fmt.Errorf("embedded schema is not valid JSON: %w", err)
		}
		if _, ok := doc["$id"].(string); !ok {
			return fmt.Errorf("embedded schema missing string $id")
		}
	}
	return nil
}
