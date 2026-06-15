package engine

import (
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/slo/spec"
	"github.com/HeaInSeo/kube-slint/pkg/slo/summary"
)

// RunMode 는 측정의 실행 컨텍스트를 정의함.
type RunMode struct {
	Location string // "inside" | "outside"
	Trigger  string // "none" | "annotation"
}

// RunConfig 는 단일 실행별 설정을 포함함.
type RunConfig struct {
	RunID      string
	StartedAt  time.Time
	FinishedAt time.Time
	Mode       RunMode

	Tags          map[string]string
	Format        string
	EvidencePaths map[string]string
}

// ExecuteRequest 는 SLO 체크 실행에 필요한 모든 데이터를 포함함.
type ExecuteRequest struct {
	Config      RunConfig
	Specs       []spec.SLISpec // core input: 직접 주입
	OutPath     string
	Reliability *summary.Reliability
	// 호환성/편의용: 레지스트리를 쓰는 호출자를 위해 남길 수 있음, 일단 주석처리함.
	// OutputPath는 요약 파일이 기록되는 경로임.
}

// MeasurementMethod 는 측정 방식을 정의함 (표준화됨).
type MeasurementMethod string

const (
	// InsideSnapshot 은 클러스터 내부에서 메트릭 스냅샷을 사용하여 SLI를 측정함.
	InsideSnapshot MeasurementMethod = "InsideSnapshot"

	// InsideAnnotation 은 이후 단계를 위해 예약됨.
	InsideAnnotation MeasurementMethod = "InsideAnnotation"
	// OutsideSnapshot 은 클러스터 외부에서 가져온 메트릭을 사용하여 SLI를 측정함.
	OutsideSnapshot MeasurementMethod = "OutsideSnapshot"
)

// RunLocation 은 측정이 실행되는 위치를 나타냄.
type RunLocation string

// RunTrigger 는 측정 캡처를 유도하는 트리거를 나타냄.
type RunTrigger string

const (
	// RunLocationInside 는 측정이 대상 환경 내부에서 실행됨을 의미함.
	RunLocationInside RunLocation = "inside"

	// RunLocationOutside 는 측정이 대상 환경 외부에서 실행됨을 의미함.
	RunLocationOutside RunLocation = "outside"

	// RunTriggerNone 은 특정 트리거가 사용되지 않음을 의미함.
	RunTriggerNone RunTrigger = "none"

	// RunTriggerAnnotation 은 어노테이션에 의해 실행이 트리거됨을 의미함.
	RunTriggerAnnotation RunTrigger = "annotation"
)

// RunModeTyped 는 해결된 실행 모드를 포함함 (RunMode의 타입 지정 버전).
type RunModeTyped struct {
	Location RunLocation
	Trigger  RunTrigger
}

// MapMethodToRunMode 는 측정 방식을 프레임워크 소유의 실행 모드로 변환함.
func MapMethodToRunMode(method MeasurementMethod) RunModeTyped {
	switch method {
	case InsideAnnotation:
		return RunModeTyped{Location: RunLocationInside, Trigger: RunTriggerAnnotation}
	case OutsideSnapshot:
		return RunModeTyped{Location: RunLocationOutside, Trigger: RunTriggerNone}
	case InsideSnapshot:
		fallthrough
	default:
		return RunModeTyped{Location: RunLocationInside, Trigger: RunTriggerNone}
	}
}
