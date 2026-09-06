package resolve

import (
	"strings"
	"testing"
)

const hex64 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func v1RawSpec(script string) string {
	return `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,` +
		`"base_image_digest":"sha256:` + hex64 + `","script":` + strconvQuote(script) + `}`
}

// strconvQuote is a tiny JSON string quoter for test literals (avoids importing strconv just
// for %q semantics; script contents here are simple).
func strconvQuote(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestSchemasWellFormed(t *testing.T) {
	if err := schemasAgree(); err != nil {
		t.Fatalf("embedded schema artifacts must be well-formed with $id: %v", err)
	}
	if !strings.Contains(string(LegacyV0SchemaJSON()), SchemaLegacyV0) {
		t.Fatal("legacy schema artifact must carry its $id")
	}
	if !strings.Contains(string(BuildRawSpecV1SchemaJSON()), SchemaBuildV1) {
		t.Fatal("v1 schema artifact must carry its $id")
	}
}

// TestResolveRawSpec_LegacyByteForByte proves the legacy path through ResolveRawSpec is
// byte-for-byte identical to the historical Resolve, and is tagged legacy-v0 / resolve-v1.
func TestResolveRawSpec_LegacyByteForByte(t *testing.T) {
	raw := `{"image_uri":"harbor.lab.local/tool@sha256:` + hex64 + `","packages":["bwa=0.7.17"]}`
	req := Request{ToolName: "bwa", Version: "0.7.17", RawSpec: raw}

	legacy, err := Resolve(req, Context{})
	if err != nil {
		t.Fatalf("legacy Resolve: %v", err)
	}
	got, prov, err := ResolveRawSpec(req, Context{})
	if err != nil {
		t.Fatalf("ResolveRawSpec(legacy): %v", err)
	}
	if got != legacy {
		t.Fatalf("legacy resolution diverged:\n got=%+v\nwant=%+v", got, legacy)
	}
	if prov.SchemaVersion != SchemaLegacyV0 || prov.DerivationVersion != DerivationV1 {
		t.Fatalf("legacy provenance = %+v, want legacy-v0/resolve-v1", prov)
	}
}

// TestResolveRawSpec_V1Deterministic proves v1 parse+digest is invariant to key order,
// insignificant whitespace, and base-digest hex case.
func TestResolveRawSpec_V1Deterministic(t *testing.T) {
	req := Request{ToolName: "fn", Version: "1", RawSpec: v1RawSpec("#!/bin/sh\necho hi")}
	base, prov, err := ResolveRawSpec(req, Context{})
	if err != nil {
		t.Fatalf("ResolveRawSpec(v1): %v", err)
	}
	if prov.SchemaVersion != SchemaBuildV1 || prov.DerivationVersion != DerivationV1 {
		t.Fatalf("v1 provenance = %+v, want build.v1/resolve-v1", prov)
	}
	if base.BaseImageDigest != "sha256:"+hex64 {
		t.Fatalf("v1 base digest = %q", base.BaseImageDigest)
	}

	// Reordered keys + extra whitespace + UPPERCASE hex → identical identity.
	reordered := "  {\n\"kind\": 2 ,\t\"script\":\"#!/bin/sh\\necho hi\", \"base_image_digest\" : \"sha256:" +
		strings.ToUpper(hex64) + "\",\"schema_version\":\"nodevault.build.raw_spec.v1\"}\n"
	variant, _, err := ResolveRawSpec(Request{ToolName: "fn", Version: "1", RawSpec: reordered}, Context{})
	if err != nil {
		t.Fatalf("ResolveRawSpec(v1 variant): %v", err)
	}
	if variant.ToolSpecDigest != base.ToolSpecDigest {
		t.Fatal("v1 identity must be invariant to key order / whitespace / hex case")
	}
	if variant.RecipeInputsDigest != base.RecipeInputsDigest {
		t.Fatal("v1 recipe digest must be invariant to key order / whitespace / hex case")
	}
}

// TestResolveRawSpec_V1DistinctFromLegacy proves a v1 raw_spec never collides with a legacy
// one (schema_version + kind are in the identity content).
func TestResolveRawSpec_V1DistinctFromLegacy(t *testing.T) {
	v1, _, err := ResolveRawSpec(Request{ToolName: "fn", RawSpec: v1RawSpec("x")}, Context{})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	legacy, err := Resolve(Request{ToolName: "fn", RawSpec: `{"image_uri":"r@sha256:` + hex64 + `"}`}, Context{})
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	if v1.ToolSpecDigest == legacy.ToolSpecDigest {
		t.Fatal("v1 and legacy identities must not collide")
	}
}

// TestParseRawSpecV1_AcceptsIntegerKindRepresentations proves the runtime parser accepts every
// JSON representation the schema (draft 2020-12) treats as the integer 2 (2, 2.0, 2e0), matching
// the machine-checkable contract, and that all such representations converge to one v1 identity
// because kind is normalized to the enum int before digesting.
func TestParseRawSpecV1_AcceptsIntegerKindRepresentations(t *testing.T) {
	canonical, _, err := ResolveRawSpec(
		Request{ToolName: "fn", RawSpec: v1RawSpec("x")}, Context{})
	if err != nil {
		t.Fatalf("kind 2 baseline: %v", err)
	}
	for _, kind := range []string{"2.0", "2e0", "2.0e0"} {
		raw := `{"schema_version":"nodevault.build.raw_spec.v1","kind":` + kind +
			`,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`
		v, err := ParseRawSpecV1(raw)
		if err != nil {
			t.Fatalf("kind %s must be accepted (schema admits it): %v", kind, err)
		}
		if v.Kind != 2 {
			t.Fatalf("kind %s normalized to %d, want 2", kind, v.Kind)
		}
		got, _, err := ResolveRawSpec(Request{ToolName: "fn", RawSpec: raw}, Context{})
		if err != nil {
			t.Fatalf("kind %s resolve: %v", kind, err)
		}
		if got.ToolSpecDigest != canonical.ToolSpecDigest {
			t.Fatalf("kind %s must yield the same v1 identity as kind 2", kind)
		}
	}
}

func TestParseRawSpecV1_Rejects(t *testing.T) {
	cases := map[string]string{
		"unknown schema_version": `{"schema_version":"nodevault.build.raw_spec.v2","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"wrong kind":             `{"schema_version":"nodevault.build.raw_spec.v1","kind":1,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"unknown field":          `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":"x","extra":true}`,
		"non-integer kind":       `{"schema_version":"nodevault.build.raw_spec.v1","kind":2.5,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"string kind":            `{"schema_version":"nodevault.build.raw_spec.v1","kind":"2","base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"near-2 kind (high)":     `{"schema_version":"nodevault.build.raw_spec.v1","kind":2.0000000000000001,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"near-2 kind (low)":      `{"schema_version":"nodevault.build.raw_spec.v1","kind":1.99999999999999999,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"invalid base digest":    `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"notadigest","script":"x"}`,
		"empty script":           `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":""}`,
		"whitespace-only script": `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":"   "}`,
		"whitespace base digest": `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":" sha256:` + hex64 + ` ","script":"x"}`,
		"trailing content":       `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":"x"}{}`,
		"missing script":         `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRawSpecV1(raw); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
			// Routed through the resolver, a v1-candidate with any of these faults is an error,
			// never silently resolved as legacy.
			if _, _, err := ResolveRawSpec(Request{ToolName: "fn", RawSpec: raw}, Context{}); err == nil {
				t.Fatalf("ResolveRawSpec must reject %s (no legacy fallback)", name)
			}
		})
	}
}

// TestEffectiveProvenance covers absent-record compatibility mapping and fail-closed unknowns.
func TestEffectiveProvenance(t *testing.T) {
	// Pre-W3-PRE record (both absent) → historical legacy-v0 / resolve-v1.
	if p, err := EffectiveProvenance("", ""); err != nil ||
		p.SchemaVersion != SchemaLegacyV0 || p.DerivationVersion != DerivationV1 {
		t.Fatalf("absent provenance must map to legacy-v0/resolve-v1, got %+v err=%v", p, err)
	}
	// Known values pass through.
	if p, err := EffectiveProvenance(SchemaBuildV1, DerivationV1); err != nil || p.SchemaVersion != SchemaBuildV1 {
		t.Fatalf("known provenance must pass through, got %+v err=%v", p, err)
	}
	// Unknown future schema/derivation fail closed.
	if _, err := EffectiveProvenance("nodevault.build.raw_spec.v99", DerivationV1); err == nil {
		t.Fatal("unknown schema version must fail closed")
	}
	if _, err := EffectiveProvenance(SchemaBuildV1, "nodevault.resolve.v99"); err == nil {
		t.Fatal("unknown derivation version must fail closed")
	}
	// Half-populated provenance (exactly one present) is anomalous → fail closed, not legacy.
	if _, err := EffectiveProvenance(SchemaBuildV1, ""); err == nil {
		t.Fatal("half-populated provenance (schema only) must fail closed")
	}
	if _, err := EffectiveProvenance("", DerivationV1); err == nil {
		t.Fatal("half-populated provenance (derivation only) must fail closed")
	}
}

// TestResolveRawSpec_TrailingV1RoutesToStrictV1 proves a v1-candidate with trailing content
// is DETECTED as v1 and rejected by the strict v1 parser (trailing-content check) on the
// production ResolveRawSpec path — not misrouted to the legacy branch (adversarial P2).
func TestResolveRawSpec_TrailingV1RoutesToStrictV1(t *testing.T) {
	raw := v1RawSpec("x") + "{}"
	if !IsV1RawSpec(raw) {
		t.Fatal("a v1 document with trailing content must still be detected as a v1 candidate")
	}
	_, _, err := ResolveRawSpec(Request{ToolName: "fn", RawSpec: raw}, Context{})
	if err == nil {
		t.Fatal("trailing-content v1 must be rejected")
	}
	if !strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("trailing-content v1 must be rejected by the strict v1 parser, got: %v", err)
	}
}
