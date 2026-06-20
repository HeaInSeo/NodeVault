package resolve

import "testing"

func TestRecipeInputsDigest_Deterministic(t *testing.T) {
	first, err := RecipeInputsDigest(`{"tool_name":"bwa","version":"1","inputs":["a","b"]}`)
	if err != nil {
		t.Fatalf("RecipeInputsDigest first: %v", err)
	}
	second, err := RecipeInputsDigest(`{
		"inputs": ["a", "b"],
		"version": "1",
		"tool_name": "bwa"
	}`)
	if err != nil {
		t.Fatalf("RecipeInputsDigest second: %v", err)
	}
	if first != second {
		t.Fatalf("digest should ignore JSON field order/whitespace: %q vs %q", first, second)
	}
}

func TestBuildPlanDigest_IncludesBuilderIdentity(t *testing.T) {
	recipeDigest, err := RecipeInputsDigest(`{"tool_name":"bwa"}`)
	if err != nil {
		t.Fatalf("RecipeInputsDigest: %v", err)
	}
	first, err := BuildPlanDigest(recipeDigest, Context{BuilderIdentity: "builder-a"})
	if err != nil {
		t.Fatalf("BuildPlanDigest first: %v", err)
	}
	second, err := BuildPlanDigest(recipeDigest, Context{BuilderIdentity: "builder-b"})
	if err != nil {
		t.Fatalf("BuildPlanDigest second: %v", err)
	}
	if first == second {
		t.Fatal("builder identity must affect build plan digest")
	}
}

func TestBuildPlanDigest_IncludesBaseImageDigest(t *testing.T) {
	recipeDigest, err := RecipeInputsDigest(`{"tool_name":"bwa"}`)
	if err != nil {
		t.Fatalf("RecipeInputsDigest: %v", err)
	}
	first, err := BuildPlanDigest(recipeDigest, Context{BaseImageDigest: "sha256:111"})
	if err != nil {
		t.Fatalf("BuildPlanDigest first: %v", err)
	}
	second, err := BuildPlanDigest(recipeDigest, Context{BaseImageDigest: "sha256:222"})
	if err != nil {
		t.Fatalf("BuildPlanDigest second: %v", err)
	}
	if first == second {
		t.Fatal("base image digest must affect build plan digest")
	}
}

func TestToolSpecDigest_Stability(t *testing.T) {
	req := Request{
		ToolName: "bwa",
		Version:  "1.0.0",
		RawSpec:  `{"image_uri":"alpine:3.20@sha256:abc123","version":"1.0.0","tool_name":"bwa"}`,
	}
	first, err := ToolSpecDigest(req, Context{})
	if err != nil {
		t.Fatalf("ToolSpecDigest first: %v", err)
	}
	second, err := ToolSpecDigest(req, Context{})
	if err != nil {
		t.Fatalf("ToolSpecDigest second: %v", err)
	}
	if first != second {
		t.Fatalf("digest must be stable: %q vs %q", first, second)
	}
}

func TestToolSpecDigest_IndependentFromLegacyCasHash(t *testing.T) {
	got, err := ToolSpecDigest(Request{ToolName: "bwa", RawSpec: `{"cas_hash":"legacy","image_uri":"alpine:3.20@sha256:abc123","tool_name":"bwa"}`}, Context{})
	if err != nil {
		t.Fatalf("ToolSpecDigest: %v", err)
	}
	withoutLegacyCas, err := ToolSpecDigest(Request{ToolName: "bwa", RawSpec: `{"image_uri":"alpine:3.20@sha256:abc123","tool_name":"bwa"}`}, Context{})
	if err != nil {
		t.Fatalf("ToolSpecDigest without cas_hash: %v", err)
	}
	if got == "legacy" {
		t.Fatal("toolSpecDigest must not reuse legacy cas_hash value")
	}
	if got == withoutLegacyCas {
		t.Fatal("raw spec content still affects toolSpecDigest; cas_hash is not a magic override")
	}
}

func TestResolve_ExtractsPinnedBaseImage(t *testing.T) {
	resolved, err := Resolve(Request{
		ToolName: "bwa",
		RawSpec:  `{"base_image":"alpine:3.20@sha256:abc123","tool_name":"bwa"}`,
	}, Context{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.BaseImageRef != "alpine:3.20@sha256:abc123" {
		t.Fatalf("BaseImageRef got %q", resolved.BaseImageRef)
	}
	if resolved.BaseImageDigest != "sha256:abc123" {
		t.Fatalf("BaseImageDigest got %q", resolved.BaseImageDigest)
	}
	if resolved.RecipeInputsDigest == "" || resolved.BuildPlanDigest == "" || resolved.ToolSpecDigest == "" {
		t.Fatalf("expected all digests to be populated: %+v", resolved)
	}
}

func TestBaseImagePin_UnpinnedRefReturnsEmptyDigest(t *testing.T) {
	ref, digest := BaseImagePin(`{"image_uri":"alpine:3.20"}`)
	if ref != "alpine:3.20" {
		t.Fatalf("ref got %q", ref)
	}
	if digest != "" {
		t.Fatalf("digest got %q, want empty", digest)
	}
}

func TestResolve_UnpinnedBaseImageRejected(t *testing.T) {
	_, err := Resolve(Request{
		ToolName: "bwa",
		RawSpec:  `{"image_uri":"alpine:3.20","tool_name":"bwa"}`,
	}, Context{})
	if err == nil {
		t.Fatal("expected unpinned base image to be rejected")
	}
}

func TestResolve_MissingBaseImageRejected(t *testing.T) {
	_, err := Resolve(Request{
		ToolName: "bwa",
		RawSpec:  `{"tool_name":"bwa"}`,
	}, Context{})
	if err == nil {
		t.Fatal("expected missing base image to be rejected")
	}
}
