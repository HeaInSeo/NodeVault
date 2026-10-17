package fetch

import (
	"context"
	"time"
)

// Sample 은 특정 시점의 스냅샷 하나임.
type Sample struct {
	At     time.Time
	Values map[string]float64 // metricKey -> value
}

// MetricsFetcher 는 하나의 스냅샷을 가져오며, 구체적인 방법은 구현체에서 결정함.
// - outside: curl /metrics (Pod, port-forward, HTTP를 통해)
// - inside: localhost로 직접 HTTP 요청
// - trigger: /metrics 또는 status/log 파생 메트릭에서 가져옴
type MetricsFetcher interface {
	Fetch(ctx context.Context, at time.Time) (Sample, error)
}

// SnapshotFetcher 는 측정 창 시작 시점에 스냅샷을 미리 캡처할 수 있는 fetcher의 선택적 확장 인터페이스임.
//
// 배경: curlpod 같은 "실시간 fetcher"는 at time.Time 파라미터를 무시하고 항상 현재 상태를 반환함.
// engine.Execute()가 End() 내부에서 Fetch()를 두 번 호출하면 두 번 모두 워크로드 실행 후의 상태를 반환하여
// delta = 0이 되는 문제가 발생함 (Gap G).
//
// SnapshotFetcher를 구현하는 fetcher는 Session.Start() 시점에 PreFetch()를 통해
// 시작 스냅샷을 캡처하고, 이후 첫 번째 Fetch() 호출에서 캐시된 값을 반환할 수 있음.
//
// 이 인터페이스를 구현하지 않는 기존 fetcher(Mock, HTTP 등)는 영향을 받지 않음.
type SnapshotFetcher interface {
	MetricsFetcher
	// PreFetch 는 측정 창 시작 시점의 스냅샷을 캡처함.
	// Session.Start()에서 호출되며, 실패해도 non-fatal (kube-slint safety-first 원칙).
	PreFetch(ctx context.Context) error
}

// WindowFetcher retrieves samples over a time range rather than two discrete
// point-in-time snapshots.
//
// Intended use cases: PromQL range queries, soak analysis, burn-rate, and
// p95/p99 over a test window. WindowFetcher is optional; the existing
// MetricsFetcher path remains the default two-point engine path.
type WindowFetcher interface {
	FetchRange(ctx context.Context, start, end time.Time) ([]Sample, error)
}
