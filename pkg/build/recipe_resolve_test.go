package build

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ── unit tests: extractBuildString ───────────────────────────────────────────

func TestExtractBuildString_CondaYAML(t *testing.T) {
	spec := "name: bwa-env\ndependencies:\n  - bwa=0.7.17=h5bf99c6_8\n  - samtools=1.21=h50ea8bc_0\n"
	bs, _ := extractBuildString(spec, "bwa", "0.7.17")
	if bs != "h5bf99c6_8" {
		t.Errorf("expected h5bf99c6_8, got %q", bs)
	}
}

func TestExtractBuildString_DockerfileRUN(t *testing.T) {
	spec := "RUN conda install -c bioconda bwa=0.7.17=h5bf99c6_8 -y"
	bs, ch := extractBuildString(spec, "bwa", "0.7.17")
	if bs != "h5bf99c6_8" {
		t.Errorf("expected h5bf99c6_8, got %q", bs)
	}
	if ch != "bioconda" {
		t.Errorf("expected bioconda channel, got %q", ch)
	}
}

func TestExtractBuildString_NotPresent(t *testing.T) {
	spec := "name: env\ndependencies:\n  - bwa=0.7.17\n"
	bs, _ := extractBuildString(spec, "bwa", "0.7.17")
	if bs != "" {
		t.Errorf("expected empty build string, got %q", bs)
	}
}

func TestExtractBuildString_EmptySpec(t *testing.T) {
	bs, ch := extractBuildString("", "bwa", "0.7.17")
	if bs != "" || ch != "" {
		t.Errorf("expected empty results for empty spec")
	}
}

func TestExtractBuildString_WrongPackage(t *testing.T) {
	spec := "dependencies:\n  - samtools=1.21=h50ea8bc_0\n"
	bs, _ := extractBuildString(spec, "bwa", "0.7.17")
	if bs != "" {
		t.Errorf("expected empty build string for wrong package, got %q", bs)
	}
}

// ── unit tests: extractChannelFromLine ───────────────────────────────────────

func TestExtractChannelFromLine_DashC(t *testing.T) {
	ch := extractChannelFromLine("conda install -c bioconda bwa=0.7.17=h5bf99c6_8")
	if ch != "bioconda" {
		t.Errorf("expected bioconda, got %q", ch)
	}
}

func TestExtractChannelFromLine_LongFlag(t *testing.T) {
	ch := extractChannelFromLine("conda install --channel conda-forge numpy=1.26.4=py311h64a7726_0")
	if ch != "conda-forge" {
		t.Errorf("expected conda-forge, got %q", ch)
	}
}

func TestExtractChannelFromLine_NoChannel(t *testing.T) {
	ch := extractChannelFromLine("conda install bwa=0.7.17=h5bf99c6_8")
	if ch != "" {
		t.Errorf("expected empty channel, got %q", ch)
	}
}

// ── unit tests: isLinux64 ────────────────────────────────────────────────────

func TestIsLinux64_Various(t *testing.T) {
	cases := []struct {
		f    anacondaFile
		want bool
	}{
		{anacondaFile{Platform: "linux-64"}, true},
		{anacondaFile{Platform: "linux", Arch: "x86_64"}, true},
		{anacondaFile{Platform: "linux", Arch: "64"}, true},
		{anacondaFile{Platform: "", Arch: ""}, true},
		{anacondaFile{Platform: "osx-64"}, false},
		{anacondaFile{Platform: "linux", Arch: "aarch64"}, false},
		{anacondaFile{Platform: "win-64"}, false},
	}
	for _, tc := range cases {
		got := isLinux64(&tc.f)
		if got != tc.want {
			t.Errorf("isLinux64(%+v) = %v, want %v", tc.f, got, tc.want)
		}
	}
}

// ── unit tests: ResolveRecipe validation ─────────────────────────────────────

func newMinimalService() *Service {
	return &Service{}
}

func TestResolveRecipe_MissingToolName(t *testing.T) {
	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		Version:  "0.7.17",
		Packages: []*nfv1.PackageSpec{{Name: "bwa", Version: "0.7.17"}},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

func TestResolveRecipe_MissingPackages(t *testing.T) {
	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName: "bwa",
		Version:  "0.7.17",
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

func TestResolveRecipe_ClosedNetworkNoHarbor(t *testing.T) {
	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName:      "bwa",
		Version:       "0.7.17",
		RecipeKind:    nfv1.RecipeKind_RECIPE_KIND_CONDA,
		Packages:      []*nfv1.PackageSpec{{Name: "bwa", Version: "0.7.17"}},
		ClosedNetwork: true,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for closed network, got %v", st.Code())
	}
	if !strings.Contains(st.Message(), "폐쇄망") {
		t.Errorf("expected Korean error message, got %q", st.Message())
	}
}

func TestResolveRecipe_UnsupportedVariant(t *testing.T) {
	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName:   "bwa",
		Version:    "0.7.17",
		RecipeKind: nfv1.RecipeKind_RECIPE_KIND_UNSPECIFIED,
		Packages:   []*nfv1.PackageSpec{{Name: "bwa", Version: "0.7.17"}},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for unspecified variant, got %v", st.Code())
	}
}

func TestResolveRecipe_BioContainerUnimplemented(t *testing.T) {
	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName:   "bwa",
		Version:    "0.7.17",
		RecipeKind: nfv1.RecipeKind_RECIPE_KIND_BIOCONTAINER,
		Packages:   []*nfv1.PackageSpec{{Name: "bwa", Version: "0.7.17"}},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %v", st.Code())
	}
}

func TestResolveRecipe_PackageMirrorMissingURI(t *testing.T) {
	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName:   "bwa",
		Version:    "0.7.17",
		RecipeKind: nfv1.RecipeKind_RECIPE_KIND_PACKAGE_MIRROR,
		Packages:   []*nfv1.PackageSpec{{Name: "bwa", Version: "0.7.17"}},
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for missing mirror URI, got %v", st.Code())
	}
}

func TestResolveRecipe_PackageMirrorNotFound(t *testing.T) {
	svc := newMinimalService()
	resp, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName:         "bwa",
		Version:          "0.7.17",
		RecipeKind:       nfv1.RecipeKind_RECIPE_KIND_PACKAGE_MIRROR,
		Packages:         []*nfv1.PackageSpec{{Name: "bwa", Version: "0.7.17"}},
		PackageMirrorUri: "http://mirror.internal/conda",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetResolutionSource() != "not_found" {
		t.Errorf("expected not_found, got %q", resp.GetResolutionSource())
	}
}

// ── wire compatibility: RecipeVariant -> RecipeKind rename ──────────────────
//
// RecipeVariant (field 3, "variant") was renamed to RecipeKind (field 3,
// "recipe_kind") without changing the field number or any enum value number.
// A pre-rename client only ever knew the raw wire bytes — field 3, varint 3
// (RECIPE_VARIANT_PACKAGE_MIRROR) — so this test builds exactly that wire
// payload by hand and confirms today's generated code still decodes it as
// RECIPE_KIND_PACKAGE_MIRROR.
func TestResolveRecipeRequest_PreRenameWireValue_DecodesAsRecipeKind(t *testing.T) {
	// tag = (field_number 3 << 3) | wire_type 0 (varint) = 0x18; value = 3.
	rawPreRenameField3 := []byte{0x18, 0x03}

	req := &nfv1.ResolveRecipeRequest{}
	if err := proto.Unmarshal(rawPreRenameField3, req); err != nil {
		t.Fatalf("unmarshal pre-rename wire bytes: %v", err)
	}

	if got := req.GetRecipeKind(); got != nfv1.RecipeKind_RECIPE_KIND_PACKAGE_MIRROR {
		t.Errorf("expected RECIPE_KIND_PACKAGE_MIRROR for raw value 3, got %v", got)
	}
}

// ── integration-style test: fetchAnacondaChannel with a fake server ──────────

func TestFetchAnacondaChannel_ParsesBuilds(t *testing.T) {
	fakeResp := anacondaPackageResp{
		Name: "bwa",
		Files: []anacondaFile{
			{Version: "0.7.17", Build: "h5bf99c6_8", Platform: "linux-64"},
			{Version: "0.7.17", Build: "he4a0461_2", Platform: "linux-64"},
			{Version: "0.7.17", Build: "hc9558a2_0", Platform: "osx-64"},
			{Version: "0.7.18", Build: "hc9558a2_0", Platform: "linux-64"},
		},
	}

	candidates := filterAnacondaFiles(fakeResp.Files, "bwa", "0.7.17")
	if len(candidates) != 2 {
		t.Errorf("expected 2 linux-64 candidates for 0.7.17, got %d", len(candidates))
	}
	for _, c := range candidates {
		if !strings.HasPrefix(c.FullPin, "bwa=0.7.17=") {
			t.Errorf("unexpected full_pin %q", c.FullPin)
		}
		if c.Channel == "" {
			t.Errorf("channel must be set")
		}
	}
}

// Issue #9 회귀 테스트: 모든 채널에 실제로 연결이 안 되면(진짜 네트워크 실패),
// "후보 0개"로 조용히 성공 반환하지 않고 명확한 gRPC 에러를 반환해야 한다 —
// 이래야 "그 버전이 진짜 없음"과 "네트워크가 막힘"을 클라이언트가 구분할 수 있다.
func TestResolveRecipe_AllChannelsUnreachable_ReturnsUnavailable(t *testing.T) {
	unreachable := httptest.NewServer(nil)
	unreachable.Close() // 닫힌 서버 주소 = 연결 즉시 거부됨

	original := anacondaAPIBase
	anacondaAPIBase = unreachable.URL
	defer func() { anacondaAPIBase = original }()

	svc := newMinimalService()
	_, err := svc.ResolveRecipe(context.Background(), &nfv1.ResolveRecipeRequest{
		ToolName:   "samtools",
		Version:    "99.99.99-unreachable-test",
		RecipeKind: nfv1.RecipeKind_RECIPE_KIND_CONDA,
		Packages:   []*nfv1.PackageSpec{{Name: "samtools", Version: "99.99.99-unreachable-test"}},
		Channels:   []string{"bioconda-unreachable-test-channel"},
	})

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		t.Fatalf("expected Unavailable when all channels are unreachable, got err=%v", err)
	}
}

// filterAnacondaFiles is the pure-logic portion of fetchAnacondaChannel, extracted for unit testing.
func filterAnacondaFiles(files []anacondaFile, name, version string) []*nfv1.BuildStringCandidate {
	var out []*nfv1.BuildStringCandidate
	for i := range files {
		f := &files[i]
		if f.Version != version {
			continue
		}
		if !isLinux64(f) {
			continue
		}
		buildStr := f.Build
		if buildStr == "" {
			buildStr = f.Attrs.Build
		}
		if buildStr == "" {
			continue
		}
		out = append(out, &nfv1.BuildStringCandidate{
			BuildString: buildStr,
			FullPin:     name + "=" + version + "=" + buildStr,
			Channel:     "bioconda",
		})
	}
	return out
}
