package main

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"github.com/HeaInSeo/NodeVault/pkg/build"
	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"
)

func TestNormalizeBuildBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "default", raw: "", want: buildBackendInPodBuildah},
		{name: "canonical", raw: buildBackendInPodBuildah, want: buildBackendInPodBuildah},
		{name: "legacy alias", raw: legacyBuildBackendPodbridge, want: buildBackendInPodBuildah},
		{name: "disabled", raw: buildBackendDisabled, want: buildBackendDisabled},
		{name: "removed k8s job", raw: removedBuildBackendKubernetes, wantErr: true},
		{name: "unknown", raw: "other", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeBuildBackend(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeBuildBackend(%q) error = nil, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeBuildBackend(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeBuildBackend(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestRegisterBuildService_InPodBuildahInitFailure_DegradesToDisabled guards
// CLAUDE.md §12 (D-01/D-02): podbridge5/Buildah init failures — e.g. missing
// subuid ranges, storage.conf permission errors — must not block gRPC startup.
func TestRegisterBuildService_InPodBuildahInitFailure_DegradesToDisabled(t *testing.T) {
	orig := buildServiceConstructor
	t.Cleanup(func() { buildServiceConstructor = orig })
	buildServiceConstructor = func(
		*catalog.ToolRegistryService, *index.Store, *buildstate.Store,
		build.ReconcileTriggerer, build.SentinelEnqueuer,
	) (*build.Service, error) {
		return nil, errors.New("simulated podbridge5 init failure")
	}

	srv := grpc.NewServer()
	rc := &runtimeConfig{buildBackend: buildBackendInPodBuildah}

	if err := registerBuildService(srv, rc, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("registerBuildService() error = %v, want nil (must degrade, not fail)", err)
	}

	if _, ok := srv.GetServiceInfo()["nodevault.v1.BuildService"]; !ok {
		t.Fatalf("BuildService not registered after degrade-to-disabled fallback")
	}
}

// fakeSentinelClient is a minimal sentinelClient double for initSentinelClient
// tests: it must satisfy both build.SentinelEnqueuer (the enqueue call) and
// Close (connection lifecycle) without dialing a real gRPC target.
type fakeSentinelClient struct {
	closeCalls int
	closeErr   error
}

func (f *fakeSentinelClient) EnqueueValidationWork(
	context.Context, *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	return &nsv1.EnqueueValidationWorkResponse{JobId: "job-1", Status: "Queued"}, nil
}

func (f *fakeSentinelClient) Close() error {
	f.closeCalls++
	return f.closeErr
}

func TestInitSentinelClient_Disabled_ReturnsNilSentinelAndSkipsConstructor(t *testing.T) {
	orig := sentinelClientConstructor
	t.Cleanup(func() { sentinelClientConstructor = orig })
	called := false
	sentinelClientConstructor = func() (sentinelClient, error) {
		called = true
		return &fakeSentinelClient{}, nil
	}

	sentinel, closeFn := initSentinelClient(&runtimeConfig{sentinelEnabled: false})
	if sentinel != nil {
		t.Fatalf("sentinel = %v, want nil when NODESENTINEL_ENABLED is not \"true\"", sentinel)
	}
	if called {
		t.Fatal("sentinelClientConstructor must not be called when NodeSentinel integration is disabled")
	}
	closeFn() // must be a safe no-op
}

func TestInitSentinelClient_EnabledConstructorFails_DegradesToNilSentinel(t *testing.T) {
	orig := sentinelClientConstructor
	t.Cleanup(func() { sentinelClientConstructor = orig })
	sentinelClientConstructor = func() (sentinelClient, error) {
		return nil, errors.New("simulated dial failure")
	}

	sentinel, closeFn := initSentinelClient(&runtimeConfig{sentinelEnabled: true})
	if sentinel != nil {
		t.Fatalf("sentinel = %v, want nil when client construction fails "+
			"(a construction failure must degrade, not crash startup)", sentinel)
	}
	closeFn() // must be a safe no-op even though no client was ever created
}

func TestInitSentinelClient_Enabled_ReturnsSentinelAndClosesUnderlyingClient(t *testing.T) {
	orig := sentinelClientConstructor
	t.Cleanup(func() { sentinelClientConstructor = orig })
	fake := &fakeSentinelClient{}
	sentinelClientConstructor = func() (sentinelClient, error) { return fake, nil }

	sentinel, closeFn := initSentinelClient(&runtimeConfig{sentinelEnabled: true})
	if sentinel == nil {
		t.Fatal("sentinel = nil, want non-nil when enabled and client construction succeeds")
	}
	closeFn()
	if fake.closeCalls != 1 {
		t.Fatalf("underlying client Close() calls = %d, want 1", fake.closeCalls)
	}
}
