package main

import "testing"

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
