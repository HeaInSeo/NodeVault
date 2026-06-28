package profiler

import "strings"

// InfraFailureReason is the canonical set of infrastructure-level failure
// reasons recognised by NodeVault. When a ToolCheckRecord carries one of
// these reasons the failure reflects the K8s/network/registry layer, not the
// tool under test, so ValidationHash is not generated.
// See docs/OBSERVED_PROFILE_SPEC.md §3.4.
type InfraFailureReason string

const (
	InfraOOMKilled         InfraFailureReason = "oomkilled"
	InfraTimeout           InfraFailureReason = "timeout"
	InfraEviction          InfraFailureReason = "eviction"
	InfraSchedulingFailure InfraFailureReason = "scheduling_failure"
	InfraImagePullFailure  InfraFailureReason = "image_pull_failure"
	InfraRegistryPullError InfraFailureReason = "registry_pull_error"
	InfraSIGKILL           InfraFailureReason = "sigkill"
	InfraTransientFailure  InfraFailureReason = "transient_failure"
	InfraUnknown           InfraFailureReason = "unknown"
)

var infraFailureSet = map[InfraFailureReason]struct{}{
	InfraOOMKilled:         {},
	InfraTimeout:           {},
	InfraEviction:          {},
	InfraSchedulingFailure: {},
	InfraImagePullFailure:  {},
	InfraRegistryPullError: {},
	InfraSIGKILL:           {},
	InfraTransientFailure:  {},
}

// IsInfraFailure reports whether reason (from ToolCheckRecord.FailureReason)
// represents an infrastructure-level failure. Case-insensitive.
func IsInfraFailure(reason string) bool {
	_, ok := infraFailureSet[InfraFailureReason(strings.ToLower(reason))]
	return ok
}

// ClassifyFailure maps a FailureReason string to the matching InfraFailureReason
// constant. Returns InfraUnknown for unrecognised or empty values.
// Does NOT classify application-level failures (non-zero exit code from the tool).
func ClassifyFailure(reason string) InfraFailureReason {
	r := InfraFailureReason(strings.ToLower(reason))
	if _, ok := infraFailureSet[r]; ok {
		return r
	}
	return InfraUnknown
}

// ValidationStatus returns the canonical validationStatus string for a
// ToolCheckRecord given its failure reason:
//   - "" or non-infra reason → "app_failed" (tool itself failed)
//   - infra reason           → "infra_failed"
//
// Callers that already know the status should not use this function.
func ValidationStatusForFailure(reason string) string {
	if reason != "" && IsInfraFailure(reason) {
		return "infra_failed"
	}
	return "app_failed"
}
