package build

import (
	"strings"
	"testing"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const (
	validDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	validDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestValidateBuildRequest_AcceptsPinnedDockerfile(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nUSER app\nRUN true",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v", err)
	}
}

func TestValidateBuildRequest_RejectsUnpinnedFrom(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20\nRUN true",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "must be pinned") {
		t.Fatalf("ValidateBuildRequest error got %v, want pinned rejection", err)
	}
}

func TestValidateBuildRequest_RejectsShortDigest(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@sha256:abc123\nRUN true",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "64 hex") {
		t.Fatalf("ValidateBuildRequest error got %v, want digest format rejection", err)
	}
}

func TestValidateBuildRequest_RejectsLatestEvenWithDigest(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:latest@" + validDigestA + "\nRUN true",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("ValidateBuildRequest error got %v, want latest rejection", err)
	}
}

func TestValidateBuildRequest_RejectsEveryUnpinnedStage(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName: "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + " AS builder\n" +
			"RUN true\n" +
			"FROM busybox:1.36\n" +
			"COPY --from=builder /tmp /tmp",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "busybox") {
		t.Fatalf("ValidateBuildRequest error got %v, want second FROM rejection", err)
	}
}

func TestValidateBuildRequest_RejectsRootUsers(t *testing.T) {
	for _, user := range []string{"root", "0", "0:0", "root:root", "app:0"} {
		t.Run(user, func(t *testing.T) {
			req := &nfv1.BuildRequest{
				ToolName:          "bwa",
				DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nUSER " + user,
			}
			err := ValidateBuildRequest(req)
			if err == nil || !strings.Contains(err.Error(), "root") {
				t.Fatalf("ValidateBuildRequest error got %v, want root USER rejection", err)
			}
		})
	}
}

func TestValidateBuildRequest_RejectsUserVariables(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nUSER ${APP_USER}",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "variables") {
		t.Fatalf("ValidateBuildRequest error got %v, want variable USER rejection", err)
	}
}

func TestValidateBuildRequest_RejectsCondaInstallWithoutBuildString(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN conda install -c bioconda bwa=0.7.17 -y",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "name=version=build") {
		t.Fatalf("ValidateBuildRequest error got %v, want conda build string rejection", err)
	}
}

func TestValidateBuildRequest_AcceptsCondaInstallWithBuildString(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN micromamba install -c bioconda bwa=0.7.17=h5bf99c6_8 -y",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v", err)
	}
}

func TestValidateBuildRequest_RejectsEnvironmentSpecWithoutBuildString(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN true",
		EnvironmentSpec:   "dependencies:\n  - bioconda::bwa=0.7.17\n",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "environment_spec") {
		t.Fatalf("ValidateBuildRequest error got %v, want environment_spec rejection", err)
	}
}

func TestValidateBuildRequest_AcceptsEnvironmentSpecWithBuildString(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN true",
		EnvironmentSpec:   "dependencies:\n  - bioconda::bwa=0.7.17=h5bf99c6_8\n",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v", err)
	}
}

// ─── final-stage risky runtime tool policy (Sprint 9, AC-SB-01/02/03 partial) ─

func TestValidateBuildRequest_FinalStageRiskyTool_Rejected(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN curl -fsSL -o out https://example.com/tool",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "curl") {
		t.Fatalf("ValidateBuildRequest error got %v, want rejection naming curl", err)
	}
}

func TestValidateBuildRequest_AllowRuntimeToolsWithReason_Passes(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:                "bwa",
		DockerfileContent:       "FROM alpine:3.20@" + validDigestA + "\nRUN curl -fsSL -o out https://example.com/tool",
		AllowRuntimeTools:       []string{"curl"},
		AllowRuntimeToolsReason: "tool downloads its own plugin catalog at runtime",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v", err)
	}
}

func TestValidateBuildRequest_AllowRuntimeToolsWithoutReason_Rejected(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN curl -fsSL -o out https://example.com/tool",
		AllowRuntimeTools: []string{"curl"},
		// AllowRuntimeToolsReason intentionally left empty.
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "curl") {
		t.Fatalf("ValidateBuildRequest error got %v, want rejection naming curl (empty reason must not exempt it)", err)
	}
}

func TestValidateBuildRequest_BuildStageRiskyTool_NotFinalStage_Passes(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName: "bwa",
		DockerfileContent: "FROM golang:1.21@" + validDigestA + " AS builder\n" +
			"RUN curl -fsSL -o src.tar.gz https://example.com/src.tar.gz && tar -xzf src.tar.gz && make\n" +
			"FROM alpine:3.20@" + validDigestB + "\n" +
			"COPY --from=builder /out/bwa /usr/local/bin/bwa\n" +
			"USER app",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v (build-stage curl/make must be allowed)", err)
	}
}

func TestValidateBuildRequest_CleanFinalImage_NoCurl_Passes(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nUSER app\nRUN echo built",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v", err)
	}
}

// ─── NodeKit real-world Dockerfile shapes (adversarial review follow-up) ──────
//
// These use the exact Dockerfile shapes NodeKit's SourceBuild renderer
// produces (legacy RecipeBuildKind.SourceBuild, single-stage — NodeKit's
// RecipeRenderer.RenderSourceBuild) and the shape its
// RecipeBuildKind.SourceBuildStructured renderer produces (2-stage —
// NodeKit's docs/NODEKIT_SOURCEBUILD_STRUCTURED_INTENT_DESIGN.md §5),
// rather than synthetic minimal examples, so the policy is checked against
// what NodeKit actually emits. As of NodeKit commit e1aa822 (2026-07-13),
// R22-B/C/D are all implemented and wizard-integrated, and NodeKit warns
// authors off legacy SourceBuild in favor of source-structured (commit
// 2e9fdf7) — these tests describe NodeVault's side of that boundary, not a
// still-pending NodeKit capability.

func TestValidateBuildRequest_NodeKitLegacySourceBuildDockerfile_RejectedWithoutExemption(t *testing.T) {
	// Mirrors RecipeRenderer.RenderSourceBuild's single-stage output: BaseImage
	// doubles as both the fetch environment and the final runtime image, so
	// curl/tar/sha256sum used to fetch the source all land in the only (and
	// therefore final) stage.
	req := &nfv1.BuildRequest{
		ToolName: "bwa",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\n" +
			`RUN curl -fsSL -o source.tar.gz "https://example.com/bwa-0.7.17.tar.gz" && ` + "\\\n" +
			`    echo "` + validDigestA[len("sha256:"):] + `  source.tar.gz" | sha256sum -c - && ` + "\\\n" +
			"    tar -xzf source.tar.gz && \\\n" +
			"    make install\n" +
			"USER 1000",
	}
	err := ValidateBuildRequest(req)
	if err == nil {
		t.Fatal("expected legacy single-stage SourceBuild output to be rejected (curl/tar/make in the only stage)")
	}
	if !strings.Contains(err.Error(), "curl") {
		t.Fatalf("ValidateBuildRequest error got %v, want rejection naming curl (first risky tool in the RUN line)", err)
	}
}

func TestValidateBuildRequest_NodeKitStructuredSourceBuildDockerfile_Passes(t *testing.T) {
	// Mirrors the 2-stage template NodeKit's SourceBuildStructured renderer
	// produces (design doc §5, implemented in NodeKit commit 3117f8a): a
	// builder stage does the fetch/compile, and only its output is copied
	// into a clean runtime stage.
	req := &nfv1.BuildRequest{
		ToolName: "bwa",
		DockerfileContent: "FROM golang:1.21@" + validDigestA + " AS builder\n" +
			`RUN curl -fsSL -o source.tar.gz "https://example.com/bwa-0.7.17.tar.gz" && ` + "\\\n" +
			`    echo "` + validDigestA[len("sha256:"):] + `  source.tar.gz" | sha256sum -c - && ` + "\\\n" +
			"    tar -xzf source.tar.gz && \\\n" +
			"    make install DESTDIR=/nodekit/output\n" +
			"\n" +
			"FROM alpine:3.20@" + validDigestB + "\n" +
			"COPY --from=builder /nodekit/output/ /\n" +
			"USER 1000",
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest: %v (builder-stage curl/tar/make must be allowed; final stage has none)", err)
	}
}

func TestValidateBuildRequest_RejectsToolFunctionSpecOnDockerfileBuildPath(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:        "bwa-fn",
		Kind:            nfv1.BuildKind_BUILD_KIND_TOOLFUNCTIONSPEC,
		BaseImageDigest: validDigestB,
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "TOOLFUNCTIONSPEC") {
		t.Fatalf("ValidateBuildRequest error got %v, want unsupported ToolFunctionSpec rejection", err)
	}
}

func TestValidateBuildRequest_ToolSpecRejectsInvalidBaseImageDigestWhenPresent(t *testing.T) {
	req := &nfv1.BuildRequest{
		ToolName:          "bwa",
		Kind:              nfv1.BuildKind_BUILD_KIND_TOOLSPEC,
		BaseImageDigest:   "sha256:abc123",
		DockerfileContent: "FROM alpine:3.20@" + validDigestA + "\nRUN true",
	}
	err := ValidateBuildRequest(req)
	if err == nil || !strings.Contains(err.Error(), "64 hex") {
		t.Fatalf("ValidateBuildRequest error got %v, want base image digest rejection", err)
	}
}
