package validation

import "testing"

func TestImageRepoFromRef(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"host/project/repo:tag", "harbor.lab.local/library/bwa:latest", "harbor.lab.local/library/bwa"},
		{"host:port/project/repo:tag", "127.0.0.1:5000/library/bwa:v1.2.3", "127.0.0.1:5000/library/bwa"},
		{"no tag", "harbor.lab.local/library/bwa", "harbor.lab.local/library/bwa"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageRepoFromRef(tc.ref); got != tc.want {
				t.Errorf("imageRepoFromRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}
