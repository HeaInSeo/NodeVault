package main

import (
	"errors"
	"testing"

	"google.golang.org/grpc"

	"github.com/HeaInSeo/NodeVault/pkg/build"
	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
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
		*catalog.ToolRegistryService, *index.Store, *buildstate.Store, build.ReconcileTriggerer,
	) (*build.Service, error) {
		return nil, errors.New("simulated podbridge5 init failure")
	}

	srv := grpc.NewServer()
	rc := &runtimeConfig{buildBackend: buildBackendInPodBuildah}

	if err := registerBuildService(srv, rc, nil, nil, nil, nil); err != nil {
		t.Fatalf("registerBuildService() error = %v, want nil (must degrade, not fail)", err)
	}

	if _, ok := srv.GetServiceInfo()["nodevault.v1.BuildService"]; !ok {
		t.Fatalf("BuildService not registered after degrade-to-disabled fallback")
	}
}
