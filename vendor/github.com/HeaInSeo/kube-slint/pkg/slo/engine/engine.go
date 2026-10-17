package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/slo"
	"github.com/HeaInSeo/kube-slint/pkg/slo/fetch"
	"github.com/HeaInSeo/kube-slint/pkg/slo/spec"
	"github.com/HeaInSeo/kube-slint/pkg/slo/summary"
)

const statusComplete = "Complete"

// Logger는 pkg/slo를 klog/logr로부터 독립적으로 유지함.
// type Logger interface {
//	Logf(format string, args ...any)
// }

// Engine 은 메트릭 수집과 SLI 평가를 조율함.
type Engine struct {
	fetcher fetch.MetricsFetcher
	// Spec  registry.Registry // (옵션) 레지스트리를 쓰는 호출자를 위해 남길 수 있음, 일단 주석처리함.
	// reg     *spec.Registry
	writer summary.Writer
	logf   func(string, ...any)
}

// New 는 새로운 Engine 인스턴스를 생성함.
func New(fetcher fetch.MetricsFetcher, writer summary.Writer, l slo.Logger) *Engine {
	logf := func(string, ...any) {}
	if l != nil {
		logf = l.Logf
	}
	return &Engine{fetcher: fetcher, writer: writer, logf: logf}
}

// Execute 는 SLO 측정 및 평가 프로세스를 실행함.
func (e *Engine) Execute(ctx context.Context, req ExecuteRequest) (*summary.Summary, error) {
	cfg := req.Config
	if cfg.StartedAt.IsZero() || cfg.FinishedAt.IsZero() {
		return nil, fmt.Errorf("StartedAt/FinishedAt must be set")
	}

	rel := req.Reliability
	if rel == nil {
		rel = &summary.Reliability{}
	}

	needsPoint, needsWindow := splitSpecNeeds(req.Specs)

	var start, end fetch.Sample
	var scrapeLatencyStart int64
	if needsPoint {
		if e.fetcher == nil {
			rel.CollectionStatus = "Failed"
			rel.BlockedReason = "point MetricsFetcher is required"
			s := e.emptySummary(cfg, rel, []string{"point MetricsFetcher is required"})
			e.ensureConfidenceScore(rel)
			_ = e.writer.Write(req.OutPath, *s)
			return s, nil
		}

		// 스냅샷 수집
		realStart := time.Now()
		// startSkew는 측정 지시 시점(StartedAt)과 실제 스크래핑을 시도한 시점 간의 시차를 의미함.
		// 참고: 이는 하네스의 실행 지연을 의미하며, 오퍼레이터의 시작 지연이 아님.
		startSkew := realStart.Sub(cfg.StartedAt).Milliseconds()
		rel.StartSkewMs = &startSkew

		var err error
		start, err = e.fetcher.Fetch(ctx, cfg.StartedAt)
		scrapeLatencyStart = time.Since(realStart).Milliseconds()
		rel.ScrapeLatencyMs = &scrapeLatencyStart

		if err != nil {
			rel.CollectionStatus = "Failed"
			rel.BlockedReason = fmt.Sprintf("fetch(start) failed: %v", err)
			// 철학: "측정 실패는 테스트 실패가 아님" → 경고가 포함된 Summary 반환
			s := e.emptySummary(cfg, rel, []string{fmt.Sprintf("fetch(start) failed: %v", err)})
			e.ensureConfidenceScore(rel)
			_ = e.writer.Write(req.OutPath, *s)
			return s, nil
		}

		realEnd := time.Now()
		endSkew := realEnd.Sub(cfg.FinishedAt).Milliseconds()
		rel.EndSkewMs = &endSkew

		end, err = e.fetcher.Fetch(ctx, cfg.FinishedAt)
		scrapeLatencyEnd := time.Since(realEnd).Milliseconds()
		// ScrapeLatency는 시작과 종료 데이터 수집 지연 시간 중 최댓값임.
		maxLatency := scrapeLatencyStart
		if scrapeLatencyEnd > maxLatency {
			maxLatency = scrapeLatencyEnd
		}
		rel.ScrapeLatencyMs = &maxLatency

		if err != nil {
			rel.CollectionStatus = "Failed"
			rel.BlockedReason = fmt.Sprintf("fetch(end) failed: %v", err)
			s := e.emptySummary(cfg, rel, []string{fmt.Sprintf("fetch(end) failed: %v", err)})
			e.ensureConfidenceScore(rel)
			_ = e.writer.Write(req.OutPath, *s)
			return s, nil
		}
	}

	var windowSamples []fetch.Sample
	var windowErr error
	if needsWindow && req.WindowFetcher != nil {
		realWindow := time.Now()
		windowSamples, windowErr = req.WindowFetcher.FetchRange(ctx, cfg.StartedAt, cfg.FinishedAt)
		windowLatency := time.Since(realWindow).Milliseconds()
		if rel.ScrapeLatencyMs == nil || windowLatency > *rel.ScrapeLatencyMs {
			rel.ScrapeLatencyMs = &windowLatency
		}
		if windowErr != nil {
			rel.CollectionStatus = "Failed"
			rel.BlockedReason = fmt.Sprintf("fetch(window) failed: %v", windowErr)
		}
	}

	if rel.CollectionStatus == "" {
		rel.CollectionStatus = statusComplete
	}
	rel.EvaluationStatus = statusComplete // 초기에는 완전함으로 설정, 누락 시 부분(Partial)으로 강등됨

	sum := summary.Summary{
		SchemaVersion: summary.SchemaVersion,
		GeneratedAt:   time.Now(),
		Config: summary.RunConfig{
			RunID:      cfg.RunID,
			StartedAt:  cfg.StartedAt,
			FinishedAt: cfg.FinishedAt,
			Mode: summary.RunMode{
				Location: cfg.Mode.Location,
				Trigger:  cfg.Mode.Trigger,
			},
			Tags:          cfg.Tags,
			Format:        cfg.Format,
			EvidencePaths: cfg.EvidencePaths,
		},
		Reliability: rel,
	}

	missingSet := map[string]bool{}

	for _, s := range req.Specs {
		var r summary.SLIResult
		if isWindowMode(s.Compute.Mode) {
			switch {
			case req.WindowFetcher == nil:
				r = skippedSLI(s, "window fetcher required")
			case windowErr != nil:
				r = skippedSLI(s, fmt.Sprintf("fetch(window) failed: %v", windowErr))
			default:
				r = evalWindowSLI(s, windowSamples)
			}
		} else {
			r = evalSLI(s, start.Values, end.Values)
		}
		for _, m := range r.InputsMissing {
			missingSet[m] = true
		}
		if r.Status == summary.StatusSkip {
			rel.SkippedSLIs = append(rel.SkippedSLIs, s.ID)
		}
		sum.Results = append(sum.Results, r)
	}

	for missing := range missingSet {
		rel.MissingInputs = append(rel.MissingInputs, missing)
	}
	if len(rel.SkippedSLIs) > 0 {
		rel.EvaluationStatus = "Partial"
	}

	e.ensureConfidenceScore(rel)

	if err := e.writer.Write(req.OutPath, sum); err != nil {
		return nil, err
	}
	return &sum, nil
}

func splitSpecNeeds(specs []spec.SLISpec) (needsPoint, needsWindow bool) {
	if len(specs) == 0 {
		return true, false
	}
	for _, s := range specs {
		if isWindowMode(s.Compute.Mode) {
			needsWindow = true
		} else {
			needsPoint = true
		}
	}
	return needsPoint, needsWindow
}

// ensureConfidenceScore는 측정의 신뢰도 점수를 계산함.
// 이는 보조적인 분류(triage) 지표이며, 특정 상태 필드를 대체하지 않음.
// 규칙 (v1):
// - 1.0에서 시작함
// - CollectionStatus가 Complete이 아니면 점수는 0.0이 됨
// - EvaluationStatus가 Partial이면 -0.2 감점함
// - missingInputs 각 -0.1 감점함 (최대 -0.3)
// - skippedSLIs 각 -0.1 감점함 (최대 -0.3)
// - skew/latency가 5000을 초과하면 지표 왜곡으로 간주하여 -0.1 감점함
func (e *Engine) ensureConfidenceScore(rel *summary.Reliability) {
	if rel == nil {
		return
	}
	score := 1.0

	if rel.CollectionStatus != "Complete" {
		score = 0.0
	} else {
		if rel.EvaluationStatus == "Partial" {
			score -= 0.2
		}

		missingPenalty := float64(len(rel.MissingInputs)) * 0.1
		if missingPenalty > 0.3 {
			missingPenalty = 0.3
		}
		score -= missingPenalty

		skippedPenalty := float64(len(rel.SkippedSLIs)) * 0.1
		if skippedPenalty > 0.3 {
			skippedPenalty = 0.3
		}
		score -= skippedPenalty

		if rel.StartSkewMs != nil && *rel.StartSkewMs > 5000 {
			score -= 0.1
		}
		if rel.EndSkewMs != nil && *rel.EndSkewMs > 5000 {
			score -= 0.1
		}
		if rel.ScrapeLatencyMs != nil && *rel.ScrapeLatencyMs > 5000 {
			score -= 0.1
		}
	}

	if score < 0.0 {
		score = 0.0
	} else if score > 1.0 {
		score = 1.0
	}

	rel.ConfidenceScore = &score
}

func (e *Engine) emptySummary(cfg RunConfig, rel *summary.Reliability, warnings []string) *summary.Summary {
	return &summary.Summary{
		SchemaVersion: summary.SchemaVersion,
		GeneratedAt:   time.Now(),
		Config: summary.RunConfig{
			RunID:         cfg.RunID,
			StartedAt:     cfg.StartedAt,
			FinishedAt:    cfg.FinishedAt,
			Mode:          summary.RunMode{Location: cfg.Mode.Location, Trigger: cfg.Mode.Trigger},
			Tags:          cfg.Tags,
			Format:        cfg.Format,
			EvidencePaths: cfg.EvidencePaths,
		},
		Reliability: rel,
		Results:     []summary.SLIResult{},
		Warnings:    warnings,
	}
}

func evalSLI(s spec.SLISpec, start, end map[string]float64) summary.SLIResult {
	res := summary.SLIResult{
		ID:          s.ID,
		Title:       s.Title,
		Unit:        s.Unit,
		Kind:        s.Kind,
		Description: s.Description,
		Status:      summary.StatusPass,
	}

	used := make([]string, 0, len(s.Inputs))
	missing := make([]string, 0, len(s.Inputs))

	// v3: 단일 입력 SLI를 권장함. 여러 입력이 존재하면 이를 합산함.
	var valStart, valEnd float64
	for _, in := range s.Inputs {
		used = append(used, in.Key)
		a, okA := start[in.Key]
		b, okB := end[in.Key]
		if !okA || !okB {
			missing = append(missing, in.Key)
			continue
		}
		valStart += a
		valEnd += b
	}
	res.InputsUsed = used
	res.InputsMissing = missing

	if len(missing) > 0 {
		res.Status = summary.StatusSkip
		res.Reason = "missing input metrics"
		return res
	}

	var value float64
	switch s.Compute.Mode {
	case spec.ComputeSingle:
		fallthrough
	case spec.ComputeStart:
		value = valStart
	case spec.ComputeEnd:
		value = valEnd
	case spec.ComputeDelta:
		value = valEnd - valStart
		if value < 0 {
			// Counter reset suspected: delegate to OnCounterReset policy.
			switch s.Compute.OnCounterReset {
			case spec.CounterResetFail:
				res.Value = &value
				res.Status = summary.StatusFail
				res.Reason = "counter reset: delta < 0 (fail policy)"
			case spec.CounterResetNoGrade:
				res.Status = summary.StatusSkip
				res.Reason = "counter reset: measurement unreliable (no_grade policy)"
			case spec.CounterResetSkip:
				res.Status = summary.StatusSkip
				res.Reason = "counter reset: excluded (skip policy)"
			default: // CounterResetWarn or ""
				res.Value = &value
				res.Status = summary.StatusWarn
				res.Reason = "delta < 0 (counter reset suspected)"
			}
			return res // judge 생략 (모든 counter reset 케이스)
		}
	default:
		res.Status = summary.StatusSkip
		res.Reason = "unknown compute mode"
		return res
	}
	res.Value = &value

	if s.Judge != nil {
		res.Status, res.Reason = judge(value, s.Judge.Rules)
	}

	return res
}

func evalWindowSLI(s spec.SLISpec, samples []fetch.Sample) summary.SLIResult {
	res := baseResult(s)

	used := make([]string, 0, len(s.Inputs))
	for _, in := range s.Inputs {
		used = append(used, in.Key)
	}
	res.InputsUsed = used

	values, missing := windowValues(s.Inputs, samples)
	res.InputsMissing = missing
	if len(missing) > 0 {
		res.Status = summary.StatusSkip
		res.Reason = "missing input metrics"
		return res
	}
	if len(values) == 0 {
		res.Status = summary.StatusSkip
		res.Reason = "empty window"
		return res
	}

	var value float64
	switch s.Compute.Mode {
	case spec.ComputeWindowMin:
		value = values[0]
		for _, v := range values[1:] {
			if v < value {
				value = v
			}
		}
	case spec.ComputeWindowMax:
		value = values[0]
		for _, v := range values[1:] {
			if v > value {
				value = v
			}
		}
	case spec.ComputeWindowAvg:
		var sum float64
		for _, v := range values {
			sum += v
		}
		value = sum / float64(len(values))
	case spec.ComputeWindowP95:
		value = percentile(values, 0.95)
	case spec.ComputeWindowP99:
		value = percentile(values, 0.99)
	case spec.ComputeWindowRatio:
		ratio, ok, reason := windowRatio(s.Inputs, samples)
		if !ok {
			res.Status = summary.StatusSkip
			res.Reason = reason
			return res
		}
		value = ratio
	default:
		res.Status = summary.StatusSkip
		res.Reason = "unknown window compute mode"
		return res
	}
	res.Value = &value

	if s.Judge != nil {
		res.Status, res.Reason = judge(value, s.Judge.Rules)
	}
	return res
}

func baseResult(s spec.SLISpec) summary.SLIResult {
	return summary.SLIResult{
		ID:          s.ID,
		Title:       s.Title,
		Unit:        s.Unit,
		Kind:        s.Kind,
		Description: s.Description,
		Status:      summary.StatusPass,
	}
}

func skippedSLI(s spec.SLISpec, reason string) summary.SLIResult {
	res := baseResult(s)
	res.Status = summary.StatusSkip
	res.Reason = reason
	for _, in := range s.Inputs {
		res.InputsUsed = append(res.InputsUsed, in.Key)
	}
	return res
}

func windowValues(inputs []spec.MetricRef, samples []fetch.Sample) ([]float64, []string) {
	var values []float64
	missingSet := map[string]bool{}
	seen := map[string]bool{}
	for _, sample := range samples {
		for _, in := range inputs {
			v, ok := sample.Values[in.Key]
			if !ok {
				missingSet[in.Key] = true
				continue
			}
			seen[in.Key] = true
			values = append(values, v)
		}
	}
	var missing []string
	for _, in := range inputs {
		if !seen[in.Key] {
			missing = append(missing, in.Key)
			continue
		}
		if missingSet[in.Key] && len(samples) == 0 {
			missing = append(missing, in.Key)
		}
	}
	return values, missing
}

func percentile(values []float64, quantile float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(quantile * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func windowRatio(inputs []spec.MetricRef, samples []fetch.Sample) (float64, bool, string) {
	if len(inputs) < 2 {
		return 0, false, "window_ratio requires numerator and denominator inputs"
	}
	var numerator, denominator float64
	seenNum, seenDen := false, false
	for _, sample := range samples {
		if v, ok := sample.Values[inputs[0].Key]; ok {
			numerator += v
			seenNum = true
		}
		if v, ok := sample.Values[inputs[1].Key]; ok {
			denominator += v
			seenDen = true
		}
	}
	if !seenNum || !seenDen {
		return 0, false, "missing input metrics"
	}
	if denominator == 0 {
		return 0, false, "window_ratio denominator is zero"
	}
	return numerator / denominator, true, ""
}

func isWindowMode(mode spec.ComputeMode) bool {
	switch mode {
	case spec.ComputeWindowMin, spec.ComputeWindowMax, spec.ComputeWindowAvg, spec.ComputeWindowP95, spec.ComputeWindowP99, spec.ComputeWindowRatio:
		return true
	default:
		return false
	}
}

func judge(v float64, rules []spec.Rule) (status summary.Status, reason string) {
	// v3: fail이 warn보다 우선함
	var warn string
	for _, r := range rules {
		if !compare(v, r.Op, r.Target) {
			continue
		}
		switch r.Level {
		case spec.LevelFail:
			return summary.StatusFail, fmt.Sprintf("rule fail: value %s %v", r.Op, r.Target)
		case spec.LevelWarn:
			warn = fmt.Sprintf("rule warn: value %s %v", r.Op, r.Target)
		default:
			// Step 6 후보: 알 수 없는 레벨에 대해 warn/skip 처리 여부 결정
		}
	}
	if warn != "" {
		return summary.StatusWarn, warn
	}
	return summary.StatusPass, ""
}

func compare(v float64, op spec.Op, target float64) bool {
	switch op {
	case spec.OpLE:
		return v <= target
	case spec.OpGE:
		return v >= target
	case spec.OpLT:
		return v < target
	case spec.OpGT:
		return v > target
	case spec.OpEQ:
		return v == target
	default:
		return false
	}
}

// ExecuteRequestStandard 는 표준화된 요청 형태임 (이전 V4).
type ExecuteRequestStandard struct {
	Method      MeasurementMethod
	Config      RunConfig
	Specs       []spec.SLISpec
	OutPath     string
	Reliability *summary.Reliability
	// WindowFetcher is optional and only used by window compute modes.
	WindowFetcher fetch.WindowFetcher
}

// ExecuteStandard 는 표준 기본값을 적용하고 엔진에 위임함.
func ExecuteStandard(ctx context.Context, eng *Engine, req ExecuteRequestStandard) (*summary.Summary, error) {
	if req.Config.Format == "" {
		req.Config.Format = "v4"
	}
	mode := MapMethodToRunMode(req.Method)
	req.Config.Mode = RunMode{
		Location: string(mode.Location),
		Trigger:  string(mode.Trigger),
	}
	return eng.Execute(ctx, ExecuteRequest{
		Config:        req.Config,
		Specs:         req.Specs,
		OutPath:       req.OutPath,
		Reliability:   req.Reliability,
		WindowFetcher: req.WindowFetcher,
	})
}
