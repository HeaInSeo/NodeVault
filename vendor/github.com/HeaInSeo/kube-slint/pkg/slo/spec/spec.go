package spec

import (
	"fmt"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo/common/promkey"
)

// MetricRef 는 SLI에 대한 메트릭 입력을 식별함.
// The Key value is source-neutral: fetchers and SLISpec.Inputs only need to
// agree on the same string. Prometheus helpers are conveniences for one source
// type, not a requirement of the engine.
type MetricRef struct {
	Key   string
	Alias string // 선택 사항
}

// InputKey creates a MetricRef from a source-neutral input key. Use this for
// JSON/expvar/custom fetchers, mock fetchers, or any source where the key is
// already the canonical input name.
func InputKey(key string) MetricRef { return MetricRef{Key: key} }

// UnsafePromKey creates a MetricRef from a raw Prometheus text key or PromQL
// expression. Prefer PromMetric(name, labels) for ordinary Prometheus metrics
// and InputKey(key) for non-Prometheus inputs.
func UnsafePromKey(key string) MetricRef { return MetricRef{Key: key} }

// Labels 는 프로메테우스 레이블 집합을 나타냄.
type Labels map[string]string

// PromMetric 은 이름과 레이블로부터 MetricRef를 생성함.
func PromMetric(name string, labels Labels) MetricRef {
	return MetricRef{Key: promkey.Format(name, map[string]string(labels))}
}

// ComputeMode 는 SLI 값을 계산하는 방법을 정의함.
type ComputeMode string

const (
	// ComputeSingle 은 시작 스냅샷만 사용함.
	ComputeSingle ComputeMode = "single" // 시작 스냅샷만 사용 (레거시 v3)
	// ComputeDelta 는 종료 값과 시작 값의 차이를 사용함.
	ComputeDelta ComputeMode = "delta" // 종료 - 시작

	// ComputeStart 는 시작 값을 사용함.
	ComputeStart ComputeMode = "start"
	// ComputeEnd 는 종료 값을 사용함.
	ComputeEnd ComputeMode = "end"

	// ComputeWindowMin uses the minimum input value observed across a window.
	ComputeWindowMin ComputeMode = "window_min"
	// ComputeWindowMax uses the maximum input value observed across a window.
	ComputeWindowMax ComputeMode = "window_max"
	// ComputeWindowAvg uses the arithmetic mean of input values observed across a window.
	ComputeWindowAvg ComputeMode = "window_avg"
	// ComputeWindowP95 uses the nearest-rank p95 over input values observed across a window.
	ComputeWindowP95 ComputeMode = "window_p95"
	// ComputeWindowP99 uses the nearest-rank p99 over input values observed across a window.
	ComputeWindowP99 ComputeMode = "window_p99"
	// ComputeWindowRatio uses sum(input[0]) / sum(input[1]) over a window.
	ComputeWindowRatio ComputeMode = "window_ratio"
)

// CounterResetPolicy defines how ComputeDelta handles delta < 0 (suspected counter reset).
type CounterResetPolicy string

const (
	// CounterResetWarn is the default: StatusWarn with reason, value preserved.
	CounterResetWarn CounterResetPolicy = "warn"
	// CounterResetNoGrade treats counter reset as measurement-unreliable: StatusSkip, value cleared.
	// The gate interprets StatusSkip (no value) as NO_GRADE — blocks promotion without FAIL.
	CounterResetNoGrade CounterResetPolicy = "no_grade"
	// CounterResetFail treats counter reset as a hard failure: StatusFail, value preserved.
	CounterResetFail CounterResetPolicy = "fail"
	// CounterResetSkip silently excludes the SLI: StatusSkip, value cleared.
	CounterResetSkip CounterResetPolicy = "skip"
)

// ComputeSpec 은 SLI 계산 방법을 설명함.
type ComputeSpec struct {
	Mode           ComputeMode
	OnCounterReset CounterResetPolicy // only meaningful for ComputeDelta; default: CounterResetWarn
}

// Level 은 규칙 위반의 심각도를 정의함.
type Level string

const (
	// LevelWarn 은 경고를 나타냄.
	LevelWarn Level = "warn"
	// LevelFail 은 실패를 나타냄.
	LevelFail Level = "fail"
)

// Op 는 비교 연산자를 정의함.
type Op string

const (
	// OpLE 는 작거나 같음을 의미함 (<=).
	OpLE Op = "<="
	// OpGE 는 크거나 같음을 의미함 (>=).
	OpGE Op = ">="
	// OpLT 는 작음을 의미함 (<).
	OpLT Op = "<"
	// OpGT 는 큼을 의미함 (>).
	OpGT Op = ">"
	// OpEQ 는 같음을 의미함 (==).
	OpEQ Op = "=="
)

// UnmarshalText 는 텍스트에서 연산자를 디코딩함.
func (o *Op) UnmarshalText(text []byte) error {
	op, ok := NormalizeOp(string(text))
	if !ok {
		return fmt.Errorf("invalid op: %q", string(text))
	}
	*o = op
	return nil
}

// Rule 은 v3를 위한 작은 평가 규칙임.
type Rule struct {
	Metric string  // v3에서는 일반적으로 "value"
	Op     Op      // Op LE/OpGE/...
	Target float64 // 임계값
	Level  Level   // Level Warn | LevelFail
}

// JudgeSpec 은 SLI 값을 판단하기 위한 규칙을 정의함.
type JudgeSpec struct {
	Rules []Rule
}

// SLISpec 은 선언적 SLI 정의임.
// v3에서는 의도적으로 작게 유지됨.
type SLISpec struct {
	ID          string
	Title       string
	Unit        string
	Kind        string // "delta_counter" | "gauge" | "derived" (v3 minimal)
	Description string

	Inputs  []MetricRef
	Compute ComputeSpec
	Judge   *JudgeSpec
}

// NormalizeOp 는 문자열을 Op로 파싱함.
func NormalizeOp(s string) (Op, bool) {
	t := strings.TrimSpace(strings.ToLower(s))

	switch t {
	case "<=", "=<":
		return OpLE, true
	case ">=", "=>":
		return OpGE, true
	case "<":
		return OpLT, true
	case ">":
		return OpGT, true
	case "==", "=":
		return OpEQ, true

	// 선택: 사람 친화 별칭
	case "le", "lte":
		return OpLE, true
	case "ge", "gte":
		return OpGE, true
	case "lt":
		return OpLT, true
	case "gt":
		return OpGT, true
	case "eq":
		return OpEQ, true
	case "\u2264": // ≤
		return OpLE, true
	case "\u2265": // ≥
		return OpGE, true
	default:
		return "", false
	}
}
