package resolve

import (
	"strings"
	"testing"
)

const hex64 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func v1RawSpec(baseDigestHex, script string) string {
	return `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,` +
		`"base_image_digest":"sha256:` + baseDigestHex + `","script":` + strconvQuote(script) + `}`
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
	req := Request{ToolName: "fn", Version: "1", RawSpec: v1RawSpec(hex64, "#!/bin/sh\necho hi")}
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
	v1, _, err := ResolveRawSpec(Request{ToolName: "fn", RawSpec: v1RawSpec(hex64, "x")}, Context{})
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

func TestParseRawSpecV1_Rejects(t *testing.T) {
	cases := map[string]string{
		"unknown schema_version": `{"schema_version":"nodevault.build.raw_spec.v2","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"wrong kind":             `{"schema_version":"nodevault.build.raw_spec.v1","kind":1,"base_image_digest":"sha256:` + hex64 + `","script":"x"}`,
		"unknown field":          `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":"x","extra":true}`,
		"invalid base digest":    `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"notadigest","script":"x"}`,
		"empty script":           `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hex64 + `","script":""}`,
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
}
