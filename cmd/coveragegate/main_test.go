package main

import (
	"bytes"
	"os"
	"testing"
)

const sampleBaseline = `
baseline_percent: 70.0
tolerance_percent: 1.0
`

const sampleProfile = `mode: atomic
github.com/HeaInSeo/NodeVault/pkg/build/service.go:10.1,12.2 2 1
github.com/HeaInSeo/NodeVault/pkg/build/service.go:14.1,16.2 2 0
github.com/HeaInSeo/NodeVault/cmd/controlplane/main.go:20.1,22.2 4 0
github.com/HeaInSeo/NodeVault/protos/nodevault/v1/nodevault.pb.go:1.1,3.2 100 100
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/f"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestRun_WithinTolerance_Passes(t *testing.T) {
	t.Parallel()
	profilePath := writeFile(t, sampleProfile) // pkg/ only: 2 stmts covered / 4 total = 50%
	baselinePath := writeFile(t, "baseline_percent: 40.0\ntolerance_percent: 5.0\n")

	var out bytes.Buffer
	err := run([]string{"-profile", profilePath, "-baseline", baselinePath}, &out)
	if err != nil {
		t.Fatalf("run() error = %v, want nil; output:\n%s", err, out.String())
	}
}

func TestRun_BelowFloor_Fails(t *testing.T) {
	t.Parallel()
	profilePath := writeFile(t, sampleProfile) // pkg/ only: 50%
	baselinePath := writeFile(t, sampleBaseline)

	var out bytes.Buffer
	err := run([]string{"-profile", profilePath, "-baseline", baselinePath}, &out)
	if err == nil {
		t.Fatalf("run() error = nil, want a regression error; output:\n%s", out.String())
	}
}

func TestRun_ProtosAndCmdExcludedFromScope(t *testing.T) {
	t.Parallel()
	// protos/ is 100% and cmd/ is 0%; if either leaked into the pkg/ scope
	// the computed percentage would differ from the pkg/-only 50%.
	profilePath := writeFile(t, sampleProfile)
	stmts, covered, err := scopedCoverage([]byte(mustRead(t, profilePath)), "pkg")
	if err != nil {
		t.Fatalf("scopedCoverage: %v", err)
	}
	if stmts != 4 || covered != 2 {
		t.Fatalf("scopedCoverage stmts=%d covered=%d, want stmts=4 covered=2 (pkg/ only)", stmts, covered)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // t.TempDir()-scoped test fixture, not user input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestRun_MissingProfile_Fails(t *testing.T) {
	t.Parallel()
	baselinePath := writeFile(t, sampleBaseline)

	var out bytes.Buffer
	err := run([]string{"-profile", "/nonexistent/coverage.out", "-baseline", baselinePath}, &out)
	if err == nil {
		t.Fatal("run() error = nil, want error for missing profile")
	}
}

func TestRun_InvalidBaseline_Fails(t *testing.T) {
	t.Parallel()
	profilePath := writeFile(t, sampleProfile)
	baselinePath := writeFile(t, "baseline_percent: 0\ntolerance_percent: 1.0\n")

	var out bytes.Buffer
	err := run([]string{"-profile", profilePath, "-baseline", baselinePath}, &out)
	if err == nil {
		t.Fatal("run() error = nil, want error for out-of-range baseline_percent")
	}
}

func TestRun_NoMatchingScope_Fails(t *testing.T) {
	t.Parallel()
	profilePath := writeFile(t, "mode: atomic\ngithub.com/HeaInSeo/NodeVault/protos/foo.pb.go:1.1,3.2 5 5\n")
	baselinePath := writeFile(t, sampleBaseline)

	var out bytes.Buffer
	err := run([]string{"-profile", profilePath, "-baseline", baselinePath}, &out)
	if err == nil {
		t.Fatal("run() error = nil, want error when no profile line matches the scope segment")
	}
}
