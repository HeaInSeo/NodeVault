package harness

import "github.com/HeaInSeo/kube-slint/pkg/slo/spec"

// DefaultV3Specs 는 하위 호환성을 위해 유지됨.
// 기준(baseline) 프리셋 모음을 반환함.
func DefaultV3Specs() []spec.SLISpec {
	return BaselineV3Specs()
}

// BaselineV3Specs 는 확장 가능하고 재사용 가능한 프리셋 모음임:
// controller-runtime + workqueue + rest-client.
func BaselineV3Specs() []spec.SLISpec {
	return []spec.SLISpec{
		// ---------------------------
		// controller-runtime reconcile
		// ---------------------------
		{
			ID:          "reconcile_total_delta",
			Title:       "reconcile total delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: "Delta of controller_runtime_reconcile_total during the test window (all results).",
			Inputs: []spec.MetricRef{
				// 이름만 지정하는 경우 parsePrometheusText에서 값을 누적 연산(out[name]+=val)함
				spec.PromMetric("controller_runtime_reconcile_total", nil),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:          "reconcile_success_delta",
			Title:       "reconcile success delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: `Delta of controller_runtime_reconcile_total{result="success"}.`,
			Inputs: []spec.MetricRef{
				spec.PromMetric("controller_runtime_reconcile_total", spec.Labels{"result": "success"}),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:          "reconcile_error_delta",
			Title:       "reconcile error delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: `Delta of controller_runtime_reconcile_total{result="error"}.`,
			Inputs: []spec.MetricRef{
				spec.PromMetric("controller_runtime_reconcile_total", spec.Labels{"result": "error"}),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
			// 판단(judge) 규칙 예시: 에러 델타는 0이어야 함
			// Judge: &spec.JudgeSpec{Rules: []spec.Rule{{Op: spec.OpGT, Target: 0, Level: spec.LevelFail}}},
		},

		// ---------------------------
		// workqueue (controller-runtime)
		// ---------------------------
		{
			ID:          "workqueue_adds_total_delta",
			Title:       "workqueue adds total delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: "Delta of workqueue_adds_total during the test window (all queues).",
			Inputs: []spec.MetricRef{
				spec.PromMetric("workqueue_adds_total", nil),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:          "workqueue_retries_total_delta",
			Title:       "workqueue retries total delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: "Delta of workqueue_retries_total during the test window (all queues).",
			Inputs: []spec.MetricRef{
				spec.PromMetric("workqueue_retries_total", nil),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:          "workqueue_depth_end",
			Title:       "workqueue depth at end",
			Unit:        "items",
			Kind:        "gauge",
			Description: "workqueue_depth gauge snapshot at the end of the test window (all queues).",
			Inputs: []spec.MetricRef{
				spec.PromMetric("workqueue_depth", nil),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeEnd},
		},

		// ---------------------------
		// rest-client (client-go)
		// ---------------------------
		{
			ID:          "rest_client_requests_total_delta",
			Title:       "rest client requests total delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: "Delta of rest_client_requests_total during the test window (all codes/methods).",
			Inputs: []spec.MetricRef{
				spec.PromMetric("rest_client_requests_total", nil),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:          "rest_client_429_delta",
			Title:       "rest client 429 delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: `Delta of rest_client_requests_total{code="429"}. Indicates API server throttling.`,
			Inputs: []spec.MetricRef{
				spec.PromMetric("rest_client_requests_total", spec.Labels{"code": "429"}),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:          "rest_client_5xx_delta",
			Title:       "rest client 5xx delta",
			Unit:        "count",
			Kind:        "delta_counter",
			Description: `Delta of rest_client_requests_total{code="5xx"}. Some client-go versions aggregate 5xx as "5xx".`,
			Inputs: []spec.MetricRef{
				spec.PromMetric("rest_client_requests_total", spec.Labels{"code": "5xx"}),
			},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
	}
}
