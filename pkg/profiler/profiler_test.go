package profiler_test

import (
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/profiler"
)

// ── ValidationHash tests ──────────────────────────────────────────────────────

func baseCheckRecord() *index.ToolCheckRecord {
	return &index.ToolCheckRecord{
		CheckID:          "chk-1",
		ImageDigest:      "sha256:abc123",
		ToolName:         "bwa",
		Version:          "0.7.17",
		ValidationStatus: "succeeded",
		Command:          "bwa mem ref.fa reads.fastq",
		ExitCode:         0,
		ObservedIoProfile: &index.ObservedIoProfile{
			Inputs: []index.PortObservation{
				{Port: "reads", FileCount: 2, NonEmpty: true},
			},
			Outputs: []index.PortObservation{
				{Port: "alignment", FileCount: 1, NonEmpty: true},
			},
		},
		ContractCheck: &index.ContractCheck{
			AllOutputsPresent: true,
			Result:            "pass",
		},
	}
}

// TestValidationHash_Deterministic verifies same input → same hash on repeated calls.
func TestValidationHash_Deterministic(t *testing.T) {
	rec := baseCheckRecord()
	h1 := profiler.ComputeValidationHash(rec)
	h2 := profiler.ComputeValidationHash(rec)
	if h1 == "" {
		t.Fatal("ComputeValidationHash returned empty string for succeeded record")
	}
	if h1 != h2 {
		t.Errorf("non-deterministic hash: %q != %q", h1, h2)
	}
}

// TestValidationHash_ExcludesObservedResourcesByDefault verifies that changing
// environment-dependent resource fields does not affect the hash.
func TestValidationHash_ExcludesObservedResourcesByDefault(t *testing.T) {
	rec1 := baseCheckRecord()
	rec1.ObservedResourceProfile = &index.ObservedResourceProfile{
		PeakCPUMillicores: 1000,
		PeakMemoryMiB:     512,
		DurationSeconds:   42,
		DiskReadMiB:       180,
		DiskWriteMiB:      95,
	}

	rec2 := baseCheckRecord()
	rec2.ObservedResourceProfile = &index.ObservedResourceProfile{
		PeakCPUMillicores: 9999, // different node, different load
		PeakMemoryMiB:     1024,
		DurationSeconds:   120,
	}

	h1 := profiler.ComputeValidationHash(rec1)
	h2 := profiler.ComputeValidationHash(rec2)
	if h1 != h2 {
		t.Errorf("resource profile should not affect hash: %q != %q", h1, h2)
	}
}

// TestValidationHash_OnlyForSuccessfulFunctionalValidation verifies that
// non-succeeded statuses return an empty hash.
func TestValidationHash_OnlyForSuccessfulFunctionalValidation(t *testing.T) {
	cases := []struct {
		status string
	}{
		{"infra_failed"},
		{"app_failed"},
		{""},
	}
	for _, tc := range cases {
		rec := baseCheckRecord()
		rec.ValidationStatus = tc.status
		if h := profiler.ComputeValidationHash(rec); h != "" {
			t.Errorf("status=%q: expected empty hash, got %q", tc.status, h)
		}
	}
}

// TestValidationHash_NilRecord verifies that nil input returns empty string (no panic).
func TestValidationHash_NilRecord(t *testing.T) {
	if h := profiler.ComputeValidationHash(nil); h != "" {
		t.Errorf("nil record: expected empty hash, got %q", h)
	}
}

// TestValidationHash_IoPortOrderIndependent verifies that I/O port list order
// does not affect the hash (sorted before hashing).
func TestValidationHash_IoPortOrderIndependent(t *testing.T) {
	rec1 := baseCheckRecord()
	rec1.ObservedIoProfile = &index.ObservedIoProfile{
		Inputs: []index.PortObservation{
			{Port: "reads", FileCount: 2, NonEmpty: true},
			{Port: "ref", FileCount: 1, NonEmpty: true},
		},
	}

	rec2 := baseCheckRecord()
	rec2.ObservedIoProfile = &index.ObservedIoProfile{
		Inputs: []index.PortObservation{
			{Port: "ref", FileCount: 1, NonEmpty: true}, // reversed order
			{Port: "reads", FileCount: 2, NonEmpty: true},
		},
	}

	h1 := profiler.ComputeValidationHash(rec1)
	h2 := profiler.ComputeValidationHash(rec2)
	if h1 != h2 {
		t.Errorf("port order should not affect hash: %q != %q", h1, h2)
	}
}

// ── InfraFailure classifier tests ─────────────────────────────────────────────

// TestValidator_InfraFailureClassification verifies recognised infra reasons.
func TestValidator_InfraFailureClassification(t *testing.T) {
	infraCases := []string{
		"oomkilled", "OOMKilled", "OOMKILLED",
		"timeout", "Timeout",
		"eviction",
		"scheduling_failure",
		"image_pull_failure",
		"registry_pull_error",
		"sigkill",
		"transient_failure",
	}
	for _, reason := range infraCases {
		if !profiler.IsInfraFailure(reason) {
			t.Errorf("IsInfraFailure(%q) = false, want true", reason)
		}
	}
}

// TestValidator_NonInfraNotClassified verifies application failures are not infra.
func TestValidator_NonInfraNotClassified(t *testing.T) {
	appCases := []string{
		"exit_code_1",
		"assertion_failed",
		"output_missing",
		"",
		"unknown_reason_xyz",
	}
	for _, reason := range appCases {
		if profiler.IsInfraFailure(reason) {
			t.Errorf("IsInfraFailure(%q) = true, want false", reason)
		}
	}
}

// TestProfiler_TimeoutProducesInconclusiveProfile verifies that a timeout
// failure is classified as infra and produces an empty ValidationHash.
func TestProfiler_TimeoutProducesInconclusiveProfile(t *testing.T) {
	rec := &index.ToolCheckRecord{
		CheckID:          "chk-timeout",
		ImageDigest:      "sha256:def",
		ValidationStatus: "infra_failed",
		FailureReason:    "timeout",
		ObservedResourceProfile: &index.ObservedResourceProfile{
			Timeout:        true,
			TimeoutSeconds: 1800,
		},
	}

	if !profiler.IsInfraFailure(rec.FailureReason) {
		t.Errorf("timeout should be classified as infra failure")
	}

	hash := profiler.ComputeValidationHash(rec)
	if hash != "" {
		t.Errorf("infra_failed record should produce empty hash, got %q", hash)
	}

	status := profiler.ValidationStatusForFailure(rec.FailureReason)
	if status != "infra_failed" {
		t.Errorf("ValidationStatusForFailure(%q) = %q, want infra_failed", rec.FailureReason, status)
	}
}

// TestClassifyFailure_ReturnsKnownReasons verifies ClassifyFailure for known values.
func TestClassifyFailure_ReturnsKnownReasons(t *testing.T) {
	cases := []struct {
		input string
		want  profiler.InfraFailureReason
	}{
		{"oomkilled", profiler.InfraOOMKilled},
		{"timeout", profiler.InfraTimeout},
		{"eviction", profiler.InfraEviction},
		{"sigkill", profiler.InfraSIGKILL},
		{"app_panic", profiler.InfraUnknown},
		{"", profiler.InfraUnknown},
	}
	for _, tc := range cases {
		if got := profiler.ClassifyFailure(tc.input); got != tc.want {
			t.Errorf("ClassifyFailure(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
