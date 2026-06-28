// Package profiler provides utilities for computing and classifying
// L5-a validation profile results submitted by NodeSentinel.
//
// ValidationHash captures only environment-independent fields so that the
// same tool + same data + same script always produces the same hash,
// regardless of node performance or cluster load.
// See docs/OBSERVED_PROFILE_SPEC.md §3.
package profiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// hashInput is the canonical, environment-independent subset of a ToolCheckRecord
// used for ValidationHash computation. Field order is fixed for deterministic JSON.
// See docs/OBSERVED_PROFILE_SPEC.md §3.2 (included) and §3.3 (excluded).
type hashInput struct {
	Mode            string           `json:"mode"`
	ImageDigest     string           `json:"image_digest"`
	Command         string           `json:"command,omitempty"`
	ExitCode        int              `json:"exit_code"`
	IoSummary       []ioPortSummary  `json:"io_summary,omitempty"`
	ContractSummary *contractSummary `json:"contract_summary,omitempty"`
}

type ioPortSummary struct {
	Port      string `json:"port"`
	Side      string `json:"side"` // "input" | "output"
	FileCount int    `json:"file_count"`
	NonEmpty  bool   `json:"non_empty"`
}

type contractSummary struct {
	AllOutputsPresent bool   `json:"all_outputs_present"`
	Result            string `json:"result,omitempty"`
}

// ComputeValidationHash derives a deterministic SHA256 hash from the
// environment-independent fields of a successful ToolCheckRecord.
//
// Returns "" when rec == nil or rec.ValidationStatus != "succeeded".
// Excluded from hash: PeakCPUMillicores, PeakMemoryMiB, DurationSeconds,
// DiskReadMiB, DiskWriteMiB, Timeout, TimeoutSeconds, CheckedAt, FailureReason.
//
//nolint:gocritic // hugeParam: ToolCheckRecord by pointer avoids copy of slice fields.
func ComputeValidationHash(rec *index.ToolCheckRecord) string {
	if rec == nil || rec.ValidationStatus != "succeeded" {
		return ""
	}
	inp := buildHashInput(rec)
	data, _ := json.Marshal(inp)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func buildHashInput(rec *index.ToolCheckRecord) hashInput {
	inp := hashInput{
		Mode:        "dry-run",
		ImageDigest: rec.ImageDigest,
		Command:     rec.Command,
		ExitCode:    rec.ExitCode,
	}

	if rec.ObservedIoProfile != nil {
		for _, p := range rec.ObservedIoProfile.Inputs {
			inp.IoSummary = append(inp.IoSummary, ioPortSummary{
				Port: p.Port, Side: "input",
				FileCount: p.FileCount, NonEmpty: p.NonEmpty,
			})
		}
		for _, p := range rec.ObservedIoProfile.Outputs {
			inp.IoSummary = append(inp.IoSummary, ioPortSummary{
				Port: p.Port, Side: "output",
				FileCount: p.FileCount, NonEmpty: p.NonEmpty,
			})
		}
		// Sort for determinism — registry ordering is not guaranteed.
		sort.Slice(inp.IoSummary, func(i, j int) bool {
			if inp.IoSummary[i].Side != inp.IoSummary[j].Side {
				return inp.IoSummary[i].Side < inp.IoSummary[j].Side
			}
			return inp.IoSummary[i].Port < inp.IoSummary[j].Port
		})
	}

	if rec.ContractCheck != nil {
		inp.ContractSummary = &contractSummary{
			AllOutputsPresent: rec.ContractCheck.AllOutputsPresent,
			Result:            rec.ContractCheck.Result,
		}
	}

	return inp
}
