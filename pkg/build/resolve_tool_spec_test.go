package build

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const resolveTestToolName = "bwa-mem2"

func newResolveTestService(t *testing.T) *Service {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return &Service{indexStore: store}
}

func TestResolveToolSpec_FirstRequest_Succeeds(t *testing.T) {
	s := newResolveTestService(t)

	resp, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		Version:  "2.2.1",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2"}`,
	})
	if err != nil {
		t.Fatalf("ResolveToolSpec: %v", err)
	}
	if resp.GetToolSpecDigest() == "" {
		t.Fatal("expected non-empty tool_spec_digest")
	}
	if resp.GetToolName() != resolveTestToolName || resp.GetVersion() != "2.2.1" {
		t.Fatalf("unexpected response fields: %+v", resp)
	}
	if resp.GetResolvedAt() == 0 {
		t.Fatal("expected non-zero resolved_at")
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 resolved tool spec, got %d", len(specs))
	}
	if specs[0].ToolSpecDigest != resp.GetToolSpecDigest() {
		t.Fatalf("digest mismatch: store=%q resp=%q", specs[0].ToolSpecDigest, resp.GetToolSpecDigest())
	}
}

func TestResolveToolSpec_IdenticalRequest_IsIdempotent(t *testing.T) {
	s := newResolveTestService(t)
	req := &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		Version:  "2.2.1",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2"}`,
	}

	first, err := s.ResolveToolSpec(context.Background(), req)
	if err != nil {
		t.Fatalf("first ResolveToolSpec: %v", err)
	}

	second, err := s.ResolveToolSpec(context.Background(), req)
	if err != nil {
		t.Fatalf("second ResolveToolSpec: %v", err)
	}

	if first.GetToolSpecDigest() != second.GetToolSpecDigest() {
		t.Fatalf("expected same digest, got %q vs %q", first.GetToolSpecDigest(), second.GetToolSpecDigest())
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected exactly 1 resolved tool spec after duplicate request, got %d", len(specs))
	}
}

func TestResolveToolSpec_StoresInIndex(t *testing.T) {
	s := newResolveTestService(t)

	resp, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		Version:  "2.2.1",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2","version":"2.2.1"}`,
	})
	if err != nil {
		t.Fatalf("ResolveToolSpec: %v", err)
	}

	got, err := s.indexStore.GetResolvedToolSpecByDigest(resp.GetToolSpecDigest())
	if err != nil {
		t.Fatalf("GetResolvedToolSpecByDigest: %v", err)
	}
	if got.RawSpec != `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2","version":"2.2.1"}` {
		t.Fatalf("RawSpec not preserved: %q", got.RawSpec)
	}
	if got.ToolName != resolveTestToolName || got.Version != "2.2.1" {
		t.Fatalf("unexpected stored spec: %+v", got)
	}
	if got.RecipeInputsDigest == "" || got.BuildPlanDigest == "" || got.BuilderIdentity == "" {
		t.Fatalf("expected resolved digests and builder identity: %+v", got)
	}
	if got.BaseImageRef != "alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || got.BaseImageDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected base image pin: %+v", got)
	}
}

func TestResolveToolSpec_CanonicalJSON_IsDeterministic(t *testing.T) {
	s := newResolveTestService(t)

	first, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		Version:  "2.2.1",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2","version":"2.2.1"}`,
	})
	if err != nil {
		t.Fatalf("first ResolveToolSpec: %v", err)
	}
	second, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		Version:  "2.2.1",
		RawSpec: `{
			"image_uri": "alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"version": "2.2.1",
			"tool_name": "bwa-mem2"
		}`,
	})
	if err != nil {
		t.Fatalf("second ResolveToolSpec: %v", err)
	}
	if first.GetToolSpecDigest() != second.GetToolSpecDigest() {
		t.Fatalf("expected same canonical digest, got %q vs %q", first.GetToolSpecDigest(), second.GetToolSpecDigest())
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected canonical duplicate to store once, got %d records", len(specs))
	}
}

func TestResolveToolSpec_EmptyRawSpec_InvalidArgument(t *testing.T) {
	s := newResolveTestService(t)

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  "",
	})
	if err == nil {
		t.Fatal("expected error for empty raw_spec")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_EmptyToolName_InvalidArgument(t *testing.T) {
	s := newResolveTestService(t)

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error for empty tool_name")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_DifferentRawSpec_DifferentDigest(t *testing.T) {
	s := newResolveTestService(t)

	first, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2","version":"1"}`,
	})
	if err != nil {
		t.Fatalf("first ResolveToolSpec: %v", err)
	}

	second, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2","version":"2"}`,
	})
	if err != nil {
		t.Fatalf("second ResolveToolSpec: %v", err)
	}

	if first.GetToolSpecDigest() == second.GetToolSpecDigest() {
		t.Fatal("expected different digests for different raw_spec content")
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 resolved tool specs, got %d", len(specs))
	}
}

func TestResolveToolSpec_NilIndexStore_Unavailable(t *testing.T) {
	s := &Service{}

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error when indexStore is nil")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_UnpinnedBaseImage_InvalidArgument(t *testing.T) {
	s := newResolveTestService(t)

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		RawSpec:  `{"image_uri":"alpine:3.20","tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error for unpinned image_uri")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_ShortBaseImageDigest_InvalidArgument(t *testing.T) {
	s := newResolveTestService(t)

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:abc123","tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error for short image_uri digest")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

// ─── unpinned base image auto-resolution (NODEVAULT_RESOLVE_UNPINNED_BASE_IMAGE) ───

type fakeBaseImageResolver struct {
	digest string
	err    error
	gotRef string
}

func (f *fakeBaseImageResolver) ResolveTagDigest(_ context.Context, ref string) (string, error) {
	f.gotRef = ref
	return f.digest, f.err
}

func TestResolveToolSpec_UnpinnedBaseImage_FlagOff_StillRejects(t *testing.T) {
	s := newResolveTestService(t)
	s.baseImageResolver = &fakeBaseImageResolver{digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		RawSpec:  `{"image_uri":"alpine:3.20","tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error: flag is off by default, resolver must not be consulted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_UnpinnedBaseImage_FlagOn_ResolvesDigest(t *testing.T) {
	t.Setenv("NODEVAULT_RESOLVE_UNPINNED_BASE_IMAGE", "true")
	s := newResolveTestService(t)
	resolver := &fakeBaseImageResolver{digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	s.baseImageResolver = resolver

	resp, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		RawSpec:  `{"image_uri":"docker.io/library/alpine:3.20","tool_name":"bwa-mem2"}`,
	})
	if err != nil {
		t.Fatalf("ResolveToolSpec: %v", err)
	}
	if resolver.gotRef != "docker.io/library/alpine:3.20" {
		t.Errorf("resolver called with ref %q", resolver.gotRef)
	}

	got, err := s.indexStore.GetResolvedToolSpecByDigest(resp.GetToolSpecDigest())
	if err != nil {
		t.Fatalf("GetResolvedToolSpecByDigest: %v", err)
	}
	if got.BaseImageDigest != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("BaseImageDigest: got %q, want %q", got.BaseImageDigest, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	}
}

func TestResolveToolSpec_UnpinnedBaseImage_FlagOn_ResolverError_FailedPrecondition(t *testing.T) {
	t.Setenv("NODEVAULT_RESOLVE_UNPINNED_BASE_IMAGE", "true")
	s := newResolveTestService(t)
	s.baseImageResolver = &fakeBaseImageResolver{err: fmt.Errorf("registry unreachable")}

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		RawSpec:  `{"image_uri":"docker.io/library/alpine:3.20","tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error when resolver fails")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_UnpinnedBaseImage_FlagOn_AlreadyPinned_ResolverNotCalled(t *testing.T) {
	t.Setenv("NODEVAULT_RESOLVE_UNPINNED_BASE_IMAGE", "true")
	s := newResolveTestService(t)
	resolver := &fakeBaseImageResolver{digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	s.baseImageResolver = resolver

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: resolveTestToolName,
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tool_name":"bwa-mem2"}`,
	})
	if err != nil {
		t.Fatalf("ResolveToolSpec: %v", err)
	}
	if resolver.gotRef != "" {
		t.Errorf("resolver should not be called for an already-pinned ref, got %q", resolver.gotRef)
	}
}

// TestResolveToolSpec_RecordsRawSpecProvenance covers W3-PRE durable schema/derivation
// provenance: a resolved record stores the frozen raw_spec schema id + derivation id, read
// back from the store; legacy and v1 raw_specs record their respective schema ids; and a
// pre-W3-PRE record with absent provenance maps to the historical legacy-v0 / resolve-v1
// derivation (no latest-parser fallback).
func TestResolveToolSpec_RecordsRawSpecProvenance(t *testing.T) {
	const hx = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	s := newResolveTestService(t)
	ctx := context.Background()

	// Legacy raw_spec → legacy-v0 / resolve-v1.
	legacyResp, err := s.ResolveToolSpec(ctx, &nfv1.ToolSpecRequest{
		ToolName: "legacy-tool",
		RawSpec:  `{"image_uri":"harbor.lab.local/t@sha256:` + hx + `"}`,
	})
	if err != nil {
		t.Fatalf("resolve legacy: %v", err)
	}
	legacyRec, err := s.indexStore.GetResolvedToolSpecByDigest(legacyResp.GetToolSpecDigest())
	if err != nil {
		t.Fatalf("read legacy record: %v", err)
	}
	if legacyRec.RawSpecSchemaVersion != resolve.SchemaLegacyV0 || legacyRec.DerivationVersion != resolve.DerivationV1 {
		t.Fatalf("legacy provenance = %q/%q, want %q/%q",
			legacyRec.RawSpecSchemaVersion, legacyRec.DerivationVersion, resolve.SchemaLegacyV0, resolve.DerivationV1)
	}

	// v1 build raw_spec → build.raw_spec.v1 / resolve-v1.
	v1Resp, err := s.ResolveToolSpec(ctx, &nfv1.ToolSpecRequest{
		ToolName: "fn-tool",
		RawSpec:  `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hx + `","script":"#!/bin/sh\necho hi"}`,
	})
	if err != nil {
		t.Fatalf("resolve v1: %v", err)
	}
	v1Rec, err := s.indexStore.GetResolvedToolSpecByDigest(v1Resp.GetToolSpecDigest())
	if err != nil {
		t.Fatalf("read v1 record: %v", err)
	}
	if v1Rec.RawSpecSchemaVersion != resolve.SchemaBuildV1 || v1Rec.DerivationVersion != resolve.DerivationV1 {
		t.Fatalf("v1 provenance = %q/%q, want %q/%q",
			v1Rec.RawSpecSchemaVersion, v1Rec.DerivationVersion, resolve.SchemaBuildV1, resolve.DerivationV1)
	}

	// Pre-W3-PRE record (absent provenance) → historical legacy-v0 / resolve-v1.
	eff, err := resolve.EffectiveProvenance(index.ResolvedToolSpec{}.RawSpecSchemaVersion, index.ResolvedToolSpec{}.DerivationVersion)
	if err != nil || eff.SchemaVersion != resolve.SchemaLegacyV0 || eff.DerivationVersion != resolve.DerivationV1 {
		t.Fatalf("absent-provenance record must map to legacy-v0/resolve-v1, got %+v err=%v", eff, err)
	}
}

// TestResolveToolSpec_ProvenanceDurableAcrossReload proves the W3-PRE provenance fields
// survive a store reopen (the schema-6 write/load round-trip): load() must accept the
// schema-6 file and the frozen schema/derivation must read back intact.
func TestResolveToolSpec_ProvenanceDurableAcrossReload(t *testing.T) {
	const hx = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	dir := t.TempDir()
	store, err := index.NewAt(dir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	s := &Service{indexStore: store}
	resp, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "fn",
		RawSpec:  `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hx + `","script":"#!/bin/sh\necho hi"}`,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store2, err := index.NewAt(dir) // reopen: load() must accept the schema-6 file
	if err != nil {
		t.Fatalf("reopen schema-6 index: %v", err)
	}
	rec, err := store2.GetResolvedToolSpecByDigest(resp.GetToolSpecDigest())
	if err != nil {
		t.Fatalf("read after reload: %v", err)
	}
	if rec.RawSpecSchemaVersion != resolve.SchemaBuildV1 || rec.DerivationVersion != resolve.DerivationV1 {
		t.Fatalf("provenance not durable across reload: %q/%q", rec.RawSpecSchemaVersion, rec.DerivationVersion)
	}
}

// TestResolveToolSpec_UnknownProvenanceFailsClosed proves the resolve path fails closed when
// an idempotent hit returns a record carrying an unknown derivation version (e.g. written by
// a different binary) — it is not served under the current parser.
func TestResolveToolSpec_UnknownProvenanceFailsClosed(t *testing.T) {
	const hx = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	s := &Service{indexStore: store}
	raw := `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,"base_image_digest":"sha256:` + hx + `","script":"x"}`

	// Compute the digest the resolver will produce, then seed an existing record at that
	// digest carrying an UNKNOWN derivation version.
	resolved, _, err := resolve.ResolveRawSpec(resolve.Request{ToolName: "fn", RawSpec: raw}, resolve.Context{})
	if err != nil {
		t.Fatalf("precompute resolve: %v", err)
	}
	if aerr := store.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest:       resolved.ToolSpecDigest,
		RawSpecSchemaVersion: resolve.SchemaBuildV1,
		DerivationVersion:    "nodevault.resolve.v99",
	}); aerr != nil {
		t.Fatalf("seed unknown-derivation record: %v", aerr)
	}

	// Resolving the same raw_spec hits the seeded record; provenance enforcement must fail closed.
	if _, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{ToolName: "fn", RawSpec: raw}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unknown-derivation record must fail closed with FailedPrecondition, got %v", err)
	}
}
