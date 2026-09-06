package build

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const (
	fnBaseHex    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	fnBaseDigest = "sha256:" + fnBaseHex
)

func v1FunctionRawSpec(baseDigest, script string) string {
	return `{"schema_version":"nodevault.build.raw_spec.v1","kind":2,` +
		`"base_image_digest":"` + baseDigest + `","script":` + jsonQuote(script) + `}`
}

// jsonQuote is a tiny JSON string quoter for test script literals.
func jsonQuote(s string) string {
	var b strings.Builder
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

func newFunctionBuildService(t *testing.T) *Service {
	t.Helper()
	idx, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return &Service{indexStore: idx}
}

// seedV1Spec stores a v1 (kind=2) resolved record whose provenance is
// nodevault.build.raw_spec.v1, as ResolveToolSpec would.
func seedV1Spec(t *testing.T, s *Service, toolSpecDigest, script string) {
	t.Helper()
	if err := s.indexStore.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest:       toolSpecDigest,
		ToolName:             "bwa-fn",
		RawSpec:              v1FunctionRawSpec(fnBaseDigest, script),
		RawSpecSchemaVersion: resolve.SchemaBuildV1,
		DerivationVersion:    resolve.DerivationV1,
		ResolvedAt:           time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}
}

func seedBaseImage(t *testing.T, s *Service, imageDigest, imageRef string) {
	t.Helper()
	if err := s.indexStore.AppendToolImageRecord(index.ToolImageRecord{
		ImageDigest: imageDigest,
		ImageRef:    imageRef,
		BuildID:     "base-build",
		PushedAt:    time.Unix(50, 0).UTC(),
	}); err != nil {
		t.Fatalf("AppendToolImageRecord: %v", err)
	}
}

func TestFunctionBuildRequestFromResolved_Positive(t *testing.T) {
	s := newFunctionBuildService(t)
	seedBaseImage(t, s, fnBaseDigest, "registry.example:5000/library/bwa:latest")
	seedV1Spec(t, s, "fnspec-1", "#!/bin/sh\necho hi\n")
	spec, err := s.indexStore.GetResolvedToolSpecByDigest("fnspec-1")
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}

	req, err := s.functionBuildRequestFromResolved("build-1", spec)
	if err != nil {
		t.Fatalf("functionBuildRequestFromResolved: %v", err)
	}
	if req.GetKind() != nfv1.BuildKind_BUILD_KIND_TOOLFUNCTIONSPEC {
		t.Fatalf("kind = %v, want TOOLFUNCTIONSPEC", req.GetKind())
	}
	if req.GetBaseImageDigest() != fnBaseDigest {
		t.Fatalf("base digest = %q", req.GetBaseImageDigest())
	}
	df := req.GetDockerfileContent()
	// Base is pinned by EXACT digest (tag movement cannot alter the base), never by :latest.
	if !strings.Contains(df, "FROM registry.example:5000/library/bwa@"+fnBaseDigest) {
		t.Fatalf("Dockerfile must FROM the base by digest; got:\n%s", df)
	}
	if strings.Contains(df, ":latest") {
		t.Fatalf("Dockerfile must not reference the base by tag; got:\n%s", df)
	}
	if !strings.Contains(df, "echo hi") {
		t.Fatalf("Dockerfile must embed the exact script; got:\n%s", df)
	}
}

func TestFunctionBuildRequestFromResolved_FailClosed(t *testing.T) {
	t.Run("missing base image record", func(t *testing.T) {
		s := newFunctionBuildService(t)
		seedV1Spec(t, s, "fnspec-2", "echo hi")
		spec, _ := s.indexStore.GetResolvedToolSpecByDigest("fnspec-2")
		if _, err := s.functionBuildRequestFromResolved("b", spec); err == nil {
			t.Fatal("want fail-closed when the base image record is missing")
		}
	})
	t.Run("base record without locator", func(t *testing.T) {
		s := newFunctionBuildService(t)
		seedBaseImage(t, s, fnBaseDigest, "") // no ImageRef locator
		seedV1Spec(t, s, "fnspec-3", "echo hi")
		spec, _ := s.indexStore.GetResolvedToolSpecByDigest("fnspec-3")
		if _, err := s.functionBuildRequestFromResolved("b", spec); err == nil {
			t.Fatal("want fail-closed when the base image record has no locator")
		}
	})
}

func TestRenderFunctionDockerfile_Deterministic(t *testing.T) {
	ref := "registry.example:5000/library/bwa:latest"
	a, err := renderFunctionDockerfile(ref, fnBaseDigest, "echo one")
	if err != nil {
		t.Fatalf("render a: %v", err)
	}
	b, err := renderFunctionDockerfile(ref, fnBaseDigest, "echo one")
	if err != nil {
		t.Fatalf("render b: %v", err)
	}
	if a != b {
		t.Fatal("same inputs must render identical build recipes")
	}
	c, err := renderFunctionDockerfile(ref, fnBaseDigest, "echo two")
	if err != nil {
		t.Fatalf("render c: %v", err)
	}
	if a == c {
		t.Fatal("a different script must change the build recipe identity")
	}
}

func TestBaseImageByDigest(t *testing.T) {
	cases := map[string]string{
		"registry.example:5000/library/bwa:latest": "registry.example:5000/library/bwa@" + fnBaseDigest,
		"registry.example:5000/library/bwa":        "registry.example:5000/library/bwa@" + fnBaseDigest,
		"registry.example:5000/library/bwa@sha256:0000000000000000000000000000000000000000000000000000000000000000": "registry.example:5000/library/bwa@" + fnBaseDigest,
	}
	for in, want := range cases {
		if got := baseImageByDigest(in, fnBaseDigest); got != want {
			t.Fatalf("baseImageByDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFunctionDestination_NamespacedAndDeterministic(t *testing.T) {
	d1 := functionDestination("bwa-fn", "sha256:abcdef0123456789abcdef")
	d2 := functionDestination("bwa-fn", "sha256:abcdef0123456789abcdef")
	if d1 != d2 {
		t.Fatal("function destination must be deterministic")
	}
	if !strings.Contains(d1, ":toolfn-") {
		t.Fatalf("function destination must use the :toolfn- tag namespace; got %q", d1)
	}
	if strings.HasSuffix(d1, ":latest") {
		t.Fatalf("function destination must not collide with the base tool :latest tag; got %q", d1)
	}
}

// SubmitToolBuild fails closed (no build state created) for a v1 function spec whose
// base image is not a registered NodeVault tool image.
func TestSubmitToolBuild_FunctionMissingBase_FailsClosed(t *testing.T) {
	idx, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	state, err := buildstate.Open(filepath.Join(t.TempDir(), "bs.db"))
	if err != nil {
		t.Fatalf("buildstate.Open: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	svc := &Service{builder: &mockBuilder{digest: "sha256:built"}, indexStore: idx, buildState: state}
	seedV1Spec(t, svc, "fnspec-nb", "echo hi") // no base ToolImageRecord

	_, err = svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "req-nb",
		ToolSpecDigest: "fnspec-nb",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if _, getErr := svc.buildState.Get("req-nb"); getErr == nil {
		t.Fatal("no build state may be created when the function base fails closed")
	}
}

// A legacy (non-v1-provenance) raw_spec that carries "kind":2 must be rejected on
// the legacy build path — it must not reach the function path or be admitted as a
// caller-supplied Dockerfile that bypasses the ToolSpec Dockerfile policy (W3 P1).
func TestBuildRequestFromResolved_RejectsLegacyKind2(t *testing.T) {
	spec := index.ResolvedToolSpec{
		ToolSpecDigest: "legacy-k2",
		ToolName:       "sneaky",
		RawSpec: `{"tool_name":"sneaky","kind":2,"base_image_digest":"` + fnBaseDigest +
			`","dockerfile_content":"FROM x@` + fnBaseDigest + `\nRUN curl https://evil"}`,
	}
	if _, err := buildRequestFromResolved("b", spec); err == nil {
		t.Fatal("legacy raw_spec declaring kind=2 must be rejected on the legacy path")
	}
}

// W3: any byte-level script difference (e.g. CRLF vs LF) yields a distinct build
// recipe via the source-sha256 label, so byte-different declared scripts never
// collide onto one function image even though the heredoc normalizes the baked
// file's line endings.
func TestRenderFunctionDockerfile_ByteDifferencesDistinctRecipe(t *testing.T) {
	lf, err := renderFunctionDockerfile("registry.example/library/x:latest", fnBaseDigest, "echo hi\n")
	if err != nil {
		t.Fatalf("lf: %v", err)
	}
	crlf, err := renderFunctionDockerfile("registry.example/library/x:latest", fnBaseDigest, "echo hi\r\n")
	if err != nil {
		t.Fatalf("crlf: %v", err)
	}
	if lf == crlf {
		t.Fatal("CRLF vs LF scripts must produce distinct recipes (source-sha256 label)")
	}
	if !strings.Contains(lf, "io.nodevault.function.source-sha256=") {
		t.Fatal("recipe must bind the exact source hash via a label")
	}
}
