//go:build integration

// Package build integration tests require Harbor and the NodeVault Deployment
// running in the infra-lab Kubernetes cluster.
//
// infra-lab VM 클러스터 (multipass 또는 libvirt backend, 멀티노드 + 실제 VM 네트워크):
//
//	# Harbor 재개 (suspend 상태인 경우)
//	scripts/host/harbor-resume.sh
//	# NodeVault 배포 후 service port-forward를 통해 테스트
//	make deploy-infralab
//	make test-integration-infralab
//
// 자세한 내용: docs/INFRALAB_TESTING.md
package build

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const defaultIntegrationAddr = "localhost:50051"

func integrationAddr() string {
	if addr := os.Getenv("NODEVAULT_INTEGRATION_ADDR"); addr != "" {
		return addr
	}
	return defaultIntegrationAddr
}

// TestSubmitToolBuild_SimpleDockerfile is the ResolveToolSpec -> SubmitToolBuild
// -> WatchToolBuild equivalent of the removed legacy BuildAndRegister
// integration test (issue #15): a simple Dockerfile must build, push, and
// reach a Succeeded terminal event carrying the pushed digest.
func TestSubmitToolBuild_SimpleDockerfile(t *testing.T) {
	skipUnlessIntegration(t)
	client := newIntegrationBuildClient(t)

	toolName := "test-alpine-tool"
	dockerfile := "FROM alpine:3.20@" + alpine320Digest + "\n" +
		`RUN echo "hello nodevault" > /hello.txt` + "\n" +
		"USER 1000"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	digest := resolveIntegrationToolSpec(ctx, t, client, toolName, dockerfile)

	resp, err := client.SubmitToolBuild(ctx, &nfv1.SubmitToolBuildRequest{
		RequestId:      fmt.Sprintf("inttest-%d", time.Now().UnixMilli()),
		ToolSpecDigest: digest,
	})
	if err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	stream, err := client.WatchToolBuild(ctx, &nfv1.WatchToolBuildRequest{BuildId: resp.GetBuildId()})
	if err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}

	var finalDigest, finalStatus string
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream.Recv: %v", err)
		}
		t.Logf("[%s] %s", ev.GetStatus(), ev.GetMessage())
		finalStatus = ev.GetStatus()
		if ev.GetImageDigest() != "" {
			finalDigest = ev.GetImageDigest()
		}
		if finalStatus == "Succeeded" || finalStatus == "Failed" || finalStatus == "Interrupted" {
			break
		}
	}

	if finalStatus != "Succeeded" {
		t.Fatalf("final status = %q, want Succeeded", finalStatus)
	}
	if finalDigest == "" {
		t.Fatal("no digest acquired")
	}
	t.Logf("Gate passed: digest=%s", finalDigest)
}

// TestSubmitToolBuild_BadDockerfile is the ResolveToolSpec -> SubmitToolBuild
// -> WatchToolBuild equivalent of the removed legacy BuildAndRegister bad-Dockerfile
// integration test: a Dockerfile with a failing RUN command must reach a
// Failed terminal event, never Succeeded.
//
// Regression guard: the terminal status must be Failed (not a silent hang or
// a spurious Succeeded) when the in-Pod Buildah build fails.
func TestSubmitToolBuild_BadDockerfile(t *testing.T) {
	skipUnlessIntegration(t)
	client := newIntegrationBuildClient(t)

	toolName := "test-bad-dockerfile"
	// Intentionally broken: the command makes the in-Pod Buildah build fail.
	dockerfile := "FROM alpine:3.20@" + alpine320Digest + "\n" +
		"RUN nonexistent_command_xyz --fail\n" +
		"USER 1000"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	digest := resolveIntegrationToolSpec(ctx, t, client, toolName, dockerfile)

	resp, err := client.SubmitToolBuild(ctx, &nfv1.SubmitToolBuildRequest{
		RequestId:      fmt.Sprintf("inttest-bad-%d", time.Now().UnixMilli()),
		ToolSpecDigest: digest,
	})
	if err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	stream, err := client.WatchToolBuild(ctx, &nfv1.WatchToolBuildRequest{BuildId: resp.GetBuildId()})
	if err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}

	var finalStatus string
	for {
		ev, err := stream.Recv()
		if err != nil {
			// A gRPC-level error after some events is also a failure signal.
			t.Logf("stream.Recv error (may be expected on failure path): %v", err)
			break
		}
		t.Logf("[%s] %s", ev.GetStatus(), ev.GetMessage())
		finalStatus = ev.GetStatus()
		if finalStatus == "Succeeded" || finalStatus == "Failed" || finalStatus == "Interrupted" {
			break
		}
	}

	if finalStatus == "Succeeded" {
		t.Fatal("build unexpectedly succeeded with a broken Dockerfile")
	}
	if finalStatus != "Failed" {
		t.Fatalf("final status = %q, want Failed", finalStatus)
	}
	t.Log("Failure gate passed: Failed status received as expected")
}
