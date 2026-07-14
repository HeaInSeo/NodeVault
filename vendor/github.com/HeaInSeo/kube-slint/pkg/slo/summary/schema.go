// Package summary defines the sli-summary.json contract (schema slo.v3):
// the Summary/SLIResult shape every kube-slint measurement fetcher produces
// and every consumer (pkg/gate, cmd/slint-gate, pkg/slint) reads. A baseline
// file is not a distinct type — it is a plain Summary saved via WriteFile
// and reloaded via LoadFile, same as a regular measurement.
package summary

import (
	"fmt"
	"time"
)

// SchemaVersion is the single supported summary schema version.
// All measurement outputs must set schemaVersion to this value.
const SchemaVersion = "slo.v3"

// ValidateSchemaVersion returns an error if s.SchemaVersion is empty or not SchemaVersion.
func ValidateSchemaVersion(s Summary) error {
	if s.SchemaVersion == "" {
		return fmt.Errorf("schemaVersion is empty")
	}
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %q (want %q)", s.SchemaVersion, SchemaVersion)
	}
	return nil
}

// allowedStatuses is the enum of valid SLIResult.Status values.
var allowedStatuses = map[Status]bool{
	StatusPass:  true,
	StatusWarn:  true,
	StatusFail:  true,
	StatusBlock: true,
	StatusSkip:  true,
}

// Validate performs a comprehensive check of a Summary:
// schemaVersion must be supported, generatedAt must be non-zero, every
// SLIResult must have a non-empty and unique ID, and every SLIResult's
// Status must be one of the allowed enum values.
// External tools should call this after loading a summary to ensure contract compliance.
func Validate(s Summary) error {
	if err := ValidateSchemaVersion(s); err != nil {
		return err
	}
	if s.GeneratedAt.IsZero() {
		return fmt.Errorf("generatedAt is zero")
	}
	seenIDs := make(map[string]bool, len(s.Results))
	for i, r := range s.Results {
		if r.ID == "" {
			return fmt.Errorf("results[%d].id is empty", i)
		}
		if seenIDs[r.ID] {
			return fmt.Errorf("results[%d].id %q is a duplicate result ID", i, r.ID)
		}
		seenIDs[r.ID] = true
		if !allowedStatuses[r.Status] {
			return fmt.Errorf("results[%d].status %q is not a recognized status", i, r.Status)
		}
	}
	return nil
}

// Status 는 SLIResult의 정규화된 평가 상태임.
type Status string

const (
	// StatusPass 는 성공을 나타냄.
	StatusPass Status = "pass"
	// StatusWarn 은 경고를 나타냄.
	StatusWarn Status = "warn"
	// StatusFail 은 실패를 나타냄.
	StatusFail Status = "fail"
	// StatusBlock 은 측정/파이프라인 실패를 나타냄.
	StatusBlock Status = "block"
	// StatusSkip 은 체크가 생략됨을 나타냄.
	StatusSkip Status = "skip"
)

// Summary 는 계약 출력임. 모든 측정 방식은 이 스키마로 수렴해야 함.
type Summary struct {
	SchemaVersion string    `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`

	Config RunConfig `json:"config"`

	Reliability *Reliability `json:"reliability,omitempty"`

	Results  []SLIResult `json:"results"`
	Warnings []string    `json:"warnings,omitempty"`
}

// Reliability 는 측정의 진단 및 신뢰도 상태를 포함함.
type Reliability struct {
	CollectionStatus string   `json:"collectionStatus,omitempty"` // Complete | Partial | Failed
	EvaluationStatus string   `json:"evaluationStatus,omitempty"` // Complete | Partial | Failed
	BlockedReason    string   `json:"blockedReason,omitempty"`
	MissingInputs    []string `json:"missingInputs,omitempty"`
	SkippedSLIs      []string `json:"skippedSLIs,omitempty"`

	ConfidenceScore *float64 `json:"confidenceScore,omitempty"` // 0.0 ~ 1.0 보조 지표

	ConfigSourceType string `json:"configSourceType,omitempty"` // injected | env | discovered
	ConfigSourcePath string `json:"configSourcePath,omitempty"`

	StartSkewMs     *int64 `json:"startSkewMs,omitempty"`
	EndSkewMs       *int64 `json:"endSkewMs,omitempty"`
	ScrapeLatencyMs *int64 `json:"scrapeLatencyMs,omitempty"`
}

// RunConfig 는 Summary에 포함됨 (분석 도구가 측정 방식에 구애받지 않도록 함).
type RunConfig struct {
	RunID      string            `json:"runId,omitempty"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	Mode       RunMode           `json:"mode"`
	Tags       map[string]string `json:"tags,omitempty"`
	Format     string            `json:"format,omitempty"`

	// EvidencePaths는 원시 아티팩트를 가리킴 (선택 사항).
	EvidencePaths map[string]string `json:"evidencePaths,omitempty"`
}

// RunMode 는 실행이 어떻게 수행되었는지를 설명함.
type RunMode struct {
	Location string `json:"location"` // "inside" | "outside"
	Trigger  string `json:"trigger"`  // "none" | "annotation"
}

// SLIResult 는 단일 SLI의 평가 결과를 포함함.
type SLIResult struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`

	// v3: 단일 수치 결과. 향후: p50/p99 등의 필드 추가 예정.
	Value  *float64           `json:"value,omitempty"`
	Fields map[string]float64 `json:"fields,omitempty"`

	Status Status `json:"status"` // "pass" | "warn" | "fail" | "block" | "skip"

	Reason string `json:"reason,omitempty"`

	InputsUsed    []string `json:"inputsUsed,omitempty"`
	InputsMissing []string `json:"inputsMissing,omitempty"`
}

// ResultValues flattens Results into a map of SLI ID to value, omitting any
// result whose Value is nil (e.g. a skipped SLI). This is the single
// canonical way to go from a Summary to a comparable value map — gate
// evaluation, baseline diff, and baseline merge all need the same
// flattening and previously each reimplemented it separately.
func (s Summary) ResultValues() map[string]float64 {
	m := make(map[string]float64, len(s.Results))
	for _, r := range s.Results {
		if r.Value != nil {
			m[r.ID] = *r.Value
		}
	}
	return m
}

// EnsureFormat 은 schemaVersion을 보존하면서 포맷 힌트(기본값 v4)를 설정함.
func EnsureFormat(config map[string]any) map[string]any {
	if config == nil {
		config = map[string]any{}
	}
	if _, ok := config["format"]; !ok {
		config["format"] = "v4"
	}
	return config
}
