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
