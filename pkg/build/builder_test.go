package build

import "testing"

func TestDetectLayerCacheHit(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want bool
	}{
		{
			name: "cache hit present",
			log: "STEP 1/3: FROM alpine:3.20@sha256:aaaa\n" +
				"--> Using cache 4207038a878b4ff1f2249663c968010740834dea0ce16482c8943511c2b26c2e\n" +
				"STEP 2/3: RUN true\n",
			want: true,
		},
		{
			name: "no cache hit — full rebuild",
			log: "STEP 1/3: FROM alpine:3.20@sha256:aaaa\n" +
				"STEP 2/3: RUN true\n" +
				"--> abc123def456\n" +
				"COMMIT harbor.lab.local/library/tool:latest\n",
			want: false,
		},
		{
			name: "empty log",
			log:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectLayerCacheHit(tt.log); got != tt.want {
				t.Errorf("detectLayerCacheHit(%q) = %v, want %v", tt.log, got, tt.want)
			}
		})
	}
}
