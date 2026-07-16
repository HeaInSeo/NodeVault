//go:build integration

// SourceBuild policy (Sprint 9, pkg/build/validate.go) integration coverage.
// pkg/build/validate_test.go already regression-guards the static
// accept/reject decision with synthetic digests; these tests confirm the
// same NodeKit Dockerfile shapes are accepted/rejected through the real
// ResolveToolSpec -> SubmitToolBuild path against a live cluster.
//
//	make deploy-infralab
//	make test-integration-infralab
//
// See docs/INFRALAB_TESTING.md.
package build

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// alpine320Digest is docker.io/library/alpine:3.20's manifest-list digest,
// pinned because ResolveToolSpec requires a real @sha256-pinned base image
// (unlike the legacy BuildAndRegister path used elsewhere in this package).
// If this test starts failing with a pull/manifest error, re-resolve the
// current digest for the alpine:3.20 tag and update this constant.
const alpine320Digest = "sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// golang121Digest is docker.io/library/golang:1.21's manifest-list digest,
// used only by the structured-build fixture below (mirrors NodeKit's real
// SourceBuildStructured builder-stage image). See the same re-resolve note
// as alpine320Digest if this starts failing on pull.
const golang121Digest = "sha256:4746d26432a9117a5f58e95cb9f954ddf0de128e9d5816886514199316e4a2fb"

func newIntegrationBuildClient(t *testing.T) nfv1.BuildServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(integrationAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return nfv1.NewBuildServiceClient(conn)
}

// TestSubmitToolBuild_NodeKitLegacySourceBuildDockerfile_RejectedLive mirrors
// TestValidateBuildRequest_NodeKitLegacySourceBuildDockerfile_RejectedWithoutExemption
// (pkg/build/validate_test.go) but drives it through the real gRPC path
// against a live cluster: RecipeRenderer.RenderSourceBuild's single-stage
// output leaves curl/tar/make in the only (and therefore final) stage.
func TestSubmitToolBuild_NodeKitLegacySourceBuildDockerfile_RejectedLive(t *testing.T) {
	skipUnlessIntegration(t)
	client := newIntegrationBuildClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dockerfile := "FROM alpine:3.20@" + alpine320Digest + "\n" +
		`RUN curl -fsSL -o source.tar.gz "https://example.com/bwa-0.7.17.tar.gz" && ` + "\\\n" +
		`    echo "` + strings.TrimPrefix(alpine320Digest, "sha256:") + `  source.tar.gz" | sha256sum -c - && ` + "\\\n" +
		"    tar -xzf source.tar.gz && \\\n" +
		"    make install\n" +
		"USER 1000"

	digest := resolveIntegrationToolSpec(ctx, t, client, "inttest-sb-legacy", dockerfile)

	_, err := client.SubmitToolBuild(ctx, &nfv1.SubmitToolBuildRequest{
		RequestId:      fmt.Sprintf("inttest-sb-legacy-%d", time.Now().UnixMilli()),
		ToolSpecDigest: digest,
	})
	if err == nil {
		t.Fatal("SubmitToolBuild succeeded, want rejection: legacy single-stage SourceBuild output has curl/tar/make in the only stage")
	}
	if !strings.Contains(err.Error(), "curl") {
		t.Fatalf("SubmitToolBuild error = %v, want rejection naming curl (first risky tool in the RUN line)", err)
	}
	t.Logf("Gate passed: rejected as expected: %v", err)
}

// TestSubmitToolBuild_NodeKitStructuredSourceBuildDockerfile_PassesPolicyGateLive
// mirrors TestValidateBuildRequest_NodeKitStructuredSourceBuildDockerfile_Passes
// but drives it through the real gRPC path: a builder stage does the
// fetch/compile, only its output is copied into a clean runtime stage, so
// the policy gate must accept it (curl/tar/make are not in the final stage).
//
// This test only asserts the policy-gate boundary (SubmitToolBuild must not
// reject on policy grounds) — it does not wait for or require the build
// itself to reach Succeeded. golang:1.21's layers have been observed to hit
// a pre-existing, unrelated overlay/userns capability limitation under this
// cluster's hostUsers:false + chroot isolation setup (see
// docs/OVERLAY_USERNS_INVESTIGATION.md); that is a separate, already-tracked
// constraint, not a regression in this policy. The WatchToolBuild poll below
// is best-effort logging only.
func TestSubmitToolBuild_NodeKitStructuredSourceBuildDockerfile_PassesPolicyGateLive(t *testing.T) {
	skipUnlessIntegration(t)
	client := newIntegrationBuildClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dockerfile := "FROM golang:1.21@" + golang121Digest + " AS builder\n" +
		`RUN curl -fsSL -o source.tar.gz "https://example.com/bwa-0.7.17.tar.gz" && ` + "\\\n" +
		`    echo "` + strings.TrimPrefix(alpine320Digest, "sha256:") + `  source.tar.gz" | sha256sum -c - && ` + "\\\n" +
		"    tar -xzf source.tar.gz && \\\n" +
		"    make install DESTDIR=/nodekit/output\n" +
		"\n" +
		"FROM alpine:3.20@" + alpine320Digest + "\n" +
		"COPY --from=builder /nodekit/output/ /\n" +
		"USER 1000"

	digest := resolveIntegrationToolSpec(ctx, t, client, "inttest-sb-structured", dockerfile)

	resp, err := client.SubmitToolBuild(ctx, &nfv1.SubmitToolBuildRequest{
		RequestId:      fmt.Sprintf("inttest-sb-structured-%d", time.Now().UnixMilli()),
		ToolSpecDigest: digest,
	})
	if err != nil {
		t.Fatalf("SubmitToolBuild rejected structured build, want the policy gate to accept it: %v", err)
	}
	t.Logf("Gate passed: policy accepted structured build, build_id=%s status=%s", resp.GetBuildId(), resp.GetStatus())

	watchCtx, watchCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer watchCancel()
	stream, err := client.WatchToolBuild(watchCtx, &nfv1.WatchToolBuildRequest{BuildId: resp.GetBuildId()})
	if err != nil {
		t.Logf("WatchToolBuild (best-effort, not asserted): %v", err)
		return
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Logf("WatchToolBuild stream ended (best-effort, not asserted): %v", err)
			return
		}
		t.Logf("[%s] %s", ev.GetStatus(), ev.GetMessage())
	}
}

func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG not set — skipping integration test")
	}
}

// resolveIntegrationToolSpec calls ResolveToolSpec and returns the resulting
// tool_spec_digest, failing the test on error.
func resolveIntegrationToolSpec(
	ctx context.Context, t *testing.T, client nfv1.BuildServiceClient, toolName, dockerfileContent string,
) string {
	t.Helper()
	rawSpec := fmt.Sprintf(
		`{"tool_name":%q,"image_uri":"alpine:3.20@%s","dockerfile_content":%q}`,
		toolName, alpine320Digest, dockerfileContent,
	)
	resp, err := client.ResolveToolSpec(ctx, &nfv1.ToolSpecRequest{
		ToolName:    toolName,
		Version:     "0.7.17",
		RawSpec:     rawSpec,
		RequestedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("ResolveToolSpec: %v", err)
	}
	return resp.GetToolSpecDigest()
}
