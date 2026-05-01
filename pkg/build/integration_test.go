//go:build integration

// Package build integration tests require Harbor running in the infra-lab K8s cluster
// (harbor.10.113.24.96.nip.io) and NodeVault running locally as a binary.
//
// infra-lab VM 클러스터 (multipass 또는 libvirt backend, 멀티노드 + 실제 VM 네트워크):
//
//	# Harbor 재개 (suspend 상태인 경우)
//	scripts/host/harbor-resume.sh
//	# NodeVault 로컬 실행 후 테스트
//	NODEVAULT_REGISTRY_ADDR=harbor.10.113.24.96.nip.io go run ./cmd/controlplane &
//	go test -v -tags=integration ./pkg/build/... -timeout 10m
//
// 자세한 내용: docs/INFRALAB_TESTING.md
package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const nodeforgeAddr = "localhost:50051" // assumes port-forward active

func TestBuildAndRegister_SimpleDockerfile(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG not set — skipping integration test")
	}

	conn, err := grpc.NewClient(nodeforgeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := nfv1.NewBuildServiceClient(conn)

	req := &nfv1.BuildRequest{
		RequestId:        fmt.Sprintf("inttest-%d", time.Now().UnixMilli()),
		ToolDefinitionId: "test-tool-001",
		ToolName:         "test-alpine-tool",
		ImageUri:         "docker.io/library/alpine:3.19",
		DockerfileContent: `FROM alpine:3.19 AS builder
RUN echo "hello nodeforge" > /hello.txt
`,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	stream, err := client.BuildAndRegister(ctx, req)
	if err != nil {
		t.Fatalf("BuildAndRegister RPC: %v", err)
	}

	var finalDigest string
	var succeeded bool

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream.Recv: %v", err)
		}

		t.Logf("[%s] %s", ev.Kind, ev.Message)

		switch ev.Kind {
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_DIGEST_ACQUIRED:
			finalDigest = ev.Digest
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED:
			succeeded = true
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED:
			t.Fatalf("build failed: %s", ev.Message)
		}
	}

	if !succeeded {
		t.Fatal("build did not succeed")
	}
	if finalDigest == "" {
		t.Fatal("no digest acquired")
	}
	t.Logf("Gate passed: digest=%s", finalDigest)
}

// TestBuildAndRegister_BadDockerfile verifies that a Dockerfile with a failing
// RUN command causes NodeForge to emit BUILD_EVENT_KIND_FAILED and NOT succeed.
//
// Regression guard: BUILD_EVENT_KIND_FAILED must be returned (not a silent hang
// or a spurious SUCCEEDED) when the kaniko Job exits non-zero.
func TestBuildAndRegister_BadDockerfile(t *testing.T) {
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG not set — skipping integration test")
	}

	conn, err := grpc.NewClient(nodeforgeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := nfv1.NewBuildServiceClient(conn)

	req := &nfv1.BuildRequest{
		RequestId:        fmt.Sprintf("inttest-bad-%d", time.Now().UnixMilli()),
		ToolDefinitionId: "test-tool-bad",
		ToolName:         "test-bad-dockerfile",
		ImageUri:         "docker.io/library/alpine:3.19",
		// Intentionally broken: 'nonexistent_command_xyz' will make kaniko exit non-zero.
		DockerfileContent: `FROM alpine:3.19 AS builder
RUN nonexistent_command_xyz --fail
`,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	stream, err := client.BuildAndRegister(ctx, req)
	if err != nil {
		t.Fatalf("BuildAndRegister RPC: %v", err)
	}

	var gotFailed bool
	var gotSucceeded bool

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A gRPC-level error after some events is also a failure signal.
			t.Logf("stream.Recv error (may be expected on failure path): %v", err)
			gotFailed = true
			break
		}

		t.Logf("[%s] %s", ev.Kind, ev.Message)

		switch ev.Kind {
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED:
			gotFailed = true
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED:
			gotSucceeded = true
		}
	}

	if gotSucceeded {
		t.Fatal("build unexpectedly succeeded with a broken Dockerfile")
	}
	if !gotFailed {
		t.Fatal("expected BUILD_EVENT_KIND_FAILED but did not receive it")
	}
	t.Log("Failure gate passed: BUILD_EVENT_KIND_FAILED received as expected")
}
