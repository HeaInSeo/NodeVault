package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

const sampleExceptions = `
exceptions:
  - id: GO-2026-5932
    module: golang.org/x/crypto
    package: golang.org/x/crypto/openpgp
    reason: "test fixture"
    owner: "test"
    expires: "2099-01-01"
    tracking_issue: "#0"
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/f"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestRun_ReachableFindingCoveredByException_Passes(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, sampleExceptions)
	input := `{"finding":{"osv":"GO-2026-5932","trace":[{"module":"golang.org/x/crypto","version":"v0.53.0","package":"golang.org/x/crypto/openpgp/armor","function":"Decode"}]}}` + "\n"

	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("run() error = %v, want nil; output:\n%s", err, out.String())
	}
}

func TestRun_UnlistedReachableFinding_Fails(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, sampleExceptions)
	input := `{"finding":{"osv":"GO-9999-9999","trace":[{"module":"example.com/evil","version":"v1.0.0","package":"example.com/evil/bad","function":"Boom"}]}}` + "\n"

	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader(input), &out)
	if err == nil {
		t.Fatalf("run() error = nil, want failure for unlisted finding; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "GO-9999-9999") {
		t.Errorf("output missing unlisted finding id, got:\n%s", out.String())
	}
}

func TestRun_ExpiredException_Fails(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, `
exceptions:
  - id: GO-2026-5932
    module: golang.org/x/crypto
    package: golang.org/x/crypto/openpgp
    reason: "test fixture"
    owner: "test"
    expires: "2020-01-01"
    tracking_issue: "#0"
`)
	input := `{"finding":{"osv":"GO-2026-5932","trace":[{"module":"golang.org/x/crypto","version":"v0.53.0","package":"golang.org/x/crypto/openpgp","function":"Encrypt"}]}}` + "\n"

	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader(input), &out)
	if err == nil {
		t.Fatalf("run() error = nil, want failure for expired exception; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "expired") {
		t.Errorf("output missing expiry reason, got:\n%s", out.String())
	}
}

func TestRun_ModuleMismatch_Fails(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, `
exceptions:
  - id: GO-2026-5932
    module: some/other/module
    package: some/other/module/pkg
    reason: "test fixture"
    owner: "test"
    expires: "2099-01-01"
    tracking_issue: "#0"
`)
	input := `{"finding":{"osv":"GO-2026-5932","trace":[{"module":"golang.org/x/crypto","version":"v0.53.0","package":"golang.org/x/crypto/openpgp","function":"Encrypt"}]}}` + "\n"

	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader(input), &out)
	if err == nil {
		t.Fatalf("run() error = nil, want failure for module mismatch; output:\n%s", out.String())
	}
}

func TestRun_UnusedException_WarnsButDoesNotFail(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, sampleExceptions)
	// No findings at all: the sample exception is unused but there is nothing to fail on.
	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader(`{"go_version":"go1.25.12"}`+"\n"), &out)
	if err != nil {
		t.Fatalf("run() error = %v, want nil (unused exception is a warning, not a failure); output:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("output missing unused-exception warning, got:\n%s", out.String())
	}
}

func TestRun_EmptyInput_FailsAsScanError(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, sampleExceptions)
	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatalf("run() error = nil, want failure for empty scanner output")
	}
}

func TestRun_MalformedJSON_Fails(t *testing.T) {
	t.Parallel()
	excPath := writeFile(t, sampleExceptions)
	var out bytes.Buffer
	err := run([]string{"-exceptions", excPath}, strings.NewReader("not json"), &out)
	if err == nil {
		t.Fatalf("run() error = nil, want failure for malformed JSON")
	}
}

func TestPackageCovers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		exceptionPkg, findingPkg string
		want                     bool
	}{
		{"golang.org/x/crypto/openpgp", "golang.org/x/crypto/openpgp", true},
		{"golang.org/x/crypto/openpgp", "golang.org/x/crypto/openpgp/armor", true},
		{"golang.org/x/crypto/openpgp", "golang.org/x/crypto/openpgp2", false},
		{"golang.org/x/crypto/openpgp", "golang.org/x/crypto/other", false},
	}
	for _, tt := range tests {
		if got := packageCovers(tt.exceptionPkg, tt.findingPkg); got != tt.want {
			t.Errorf("packageCovers(%q, %q) = %v, want %v", tt.exceptionPkg, tt.findingPkg, got, tt.want)
		}
	}
}

func TestExpiresLayoutParses(t *testing.T) {
	t.Parallel()
	if _, err := time.Parse(expiresLayout, "2026-10-16"); err != nil {
		t.Fatalf("expiresLayout does not parse a plain YYYY-MM-DD date: %v", err)
	}
}

const sourceSymbolConfig = "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n"

func TestRun_ModuleAdvisoryClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"source_symbol_module_only_is_informational", sourceSymbolConfig + "{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", false},
		{"imported_package_still_fails", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\",\"package\":\"example.com/module/bad\"}]}}\n", true},
		{"called_symbol_still_fails", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\",\"package\":\"example.com/module/bad\",\"function\":\"Danger\"}]}}\n", true},
		{"missing_config_fails_closed", "{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", true},
		{"binary_scan_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"binary\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", true},
		{"module_scan_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"module\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", true},
		{"package_scan_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"package\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", true},
		{"missing_mode_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", true},
		{"unknown_protocol_fails_closed", "{\"config\":{\"protocol_version\":\"v9.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n", true},
		{"late_config_fails_closed", "{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n", true},
		{"repeated_config_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"}]}}\n{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n", true},
		{"module_without_version_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\"}]}}\n", true},
		{"module_with_position_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\",\"position\":{\"line\":1}}]}}\n", true},
		{"mixed_trace_fails_closed", "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-9999-9999\",\"trace\":[{\"module\":\"example.com/module\",\"version\":\"v1.0.0\"},{\"module\":\"example.com/entry\",\"version\":\"v1.0.0\"}]}}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run([]string{"-exceptions", writeFile(t, "exceptions: []\n")}, strings.NewReader(tc.input), &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("run() error = %v, want error=%t; output:\n%s", err, tc.wantErr, out.String())
			}
		})
	}
}

func TestRun_ModuleAdvisoryDoesNotHideExpiredSymbol(t *testing.T) {
	t.Parallel()
	const input = "{\"config\":{\"protocol_version\":\"v1.0.0\",\"scan_level\":\"symbol\",\"scan_mode\":\"source\"}}\n{\"finding\":{\"osv\":\"GO-2026-5932\",\"trace\":[{\"module\":\"golang.org/x/crypto\",\"version\":\"v0.57.0\"}]}}\n{\"finding\":{\"osv\":\"GO-2026-5932\",\"trace\":[{\"module\":\"golang.org/x/crypto\",\"version\":\"v0.57.0\",\"package\":\"golang.org/x/crypto/openpgp\",\"function\":\"Encrypt\"}]}}\n"
	expired := strings.ReplaceAll(sampleExceptions, "2099-01-01", "2020-01-01")
	var out bytes.Buffer
	err := run([]string{"-exceptions", writeFile(t, expired)}, strings.NewReader(input), &out)
	if err == nil || !strings.Contains(out.String(), "expired") {
		t.Fatalf("expired symbol exception must fail; error=%v output:\n%s", err, out.String())
	}
}
