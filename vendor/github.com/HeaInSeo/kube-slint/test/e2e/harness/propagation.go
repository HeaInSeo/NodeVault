package harness

import (
	"fmt"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo/summary"
)

const modeBestEffort = "BestEffort"

// CheckStrictness 는 StrictnessMode를 기반으로 측정 파이프라인의 신뢰도를 평가함.
// 'Strictness' 전파 계약을 강제함.
func CheckStrictness(cfg SessionConfig, sum *summary.Summary) error {
	if sum == nil || sum.Reliability == nil {
		return fmt.Errorf("summary or reliability is nil")
	}

	mode := cfg.StrictnessMode
	if mode == "" {
		mode = modeBestEffort // default
	}

	blockReasons := buildStrictnessBlockReasons(cfg, sum)
	return evaluateStrictnessDecision(mode, sum.Reliability, blockReasons)
}

func buildStrictnessBlockReasons(cfg SessionConfig, sum *summary.Summary) []string {
	var blockReasons []string
	for _, res := range sum.Results {
		if res.Status == summary.StatusBlock {
			blockReasons = append(blockReasons, fmt.Sprintf("SLI %s blocked: %s", res.ID, res.Reason))
		}
	}

	rel := sum.Reliability
	if rel.BlockedReason != "" {
		blockReasons = append(blockReasons, fmt.Sprintf("pipeline blocked: %s", rel.BlockedReason))
	}

	if cfg.MaxStartSkewMs > 0 && rel.StartSkewMs != nil && *rel.StartSkewMs > cfg.MaxStartSkewMs {
		msg := fmt.Sprintf("start skew (%dms) exceeded threshold (%dms)", *rel.StartSkewMs, cfg.MaxStartSkewMs)
		blockReasons = append(blockReasons, msg)
	}
	if cfg.MaxEndSkewMs > 0 && rel.EndSkewMs != nil && *rel.EndSkewMs > cfg.MaxEndSkewMs {
		msg := fmt.Sprintf("end skew (%dms) exceeded threshold (%dms)", *rel.EndSkewMs, cfg.MaxEndSkewMs)
		blockReasons = append(blockReasons, msg)
	}
	if cfg.MaxScrapeLatencyMs > 0 && rel.ScrapeLatencyMs != nil && *rel.ScrapeLatencyMs > cfg.MaxScrapeLatencyMs {
		msg := fmt.Sprintf("scrape latency (%dms) exceeded threshold (%dms)", *rel.ScrapeLatencyMs, cfg.MaxScrapeLatencyMs)
		blockReasons = append(blockReasons, msg)
	}
	return blockReasons
}

func evaluateStrictnessDecision(mode string, rel *summary.Reliability, blockReasons []string) error {
	if err := checkStrictnessModeRules(mode, rel, blockReasons); err != nil {
		return err
	}

	// strict 모드에서 명시적 블록 사유가 있으면 항상 실패 처리함
	if mode != modeBestEffort && len(blockReasons) > 0 {
		return fmt.Errorf("pipeline blocked: %s", strings.Join(blockReasons, ", "))
	}

	return nil
}

func checkStrictnessModeRules(mode string, rel *summary.Reliability, blockReasons []string) error {
	isCollectionFailed := rel.CollectionStatus == "Failed" || rel.CollectionStatus == "Partial"
	isEvaluationFailed := rel.EvaluationStatus == "Failed" || rel.EvaluationStatus == "Partial"
	reasonsStr := strings.Join(blockReasons, ", ")

	switch mode {
	case modeBestEffort:
		return nil
	case "StrictCollection":
		if isCollectionFailed {
			return fmt.Errorf("StrictCollection violation: collection status is %s, reasons: %s",
				rel.CollectionStatus, reasonsStr)
		}
	case "StrictEvaluation":
		if isCollectionFailed || isEvaluationFailed {
			return fmt.Errorf("StrictEvaluation violation: evaluation status is %s, collection status is %s, reasons: %s",
				rel.EvaluationStatus, rel.CollectionStatus, reasonsStr)
		}
	case "RequiredSLIs":
		if isCollectionFailed || isEvaluationFailed {
			return fmt.Errorf("RequiredSLIs violation: pipeline failed, reasons: %s", reasonsStr)
		}
		if len(rel.SkippedSLIs) > 0 {
			return fmt.Errorf("RequiredSLIs violation: some SLIs were skipped: %v", rel.SkippedSLIs)
		}
	default:
		if len(blockReasons) > 0 {
			return fmt.Errorf("pipeline blocked (%s): %s", mode, reasonsStr)
		}
	}
	return nil
}

// CheckGating 은 성공적으로 계산된 SLI를 게이팅 정책에 따라 평가함.
// 'GatingPolicy' 전파 계약을 강제함.
func CheckGating(cfg SessionConfig, sum *summary.Summary) error {
	if sum == nil {
		return fmt.Errorf("summary is nil")
	}

	gateOnLevel := cfg.GateOnLevel
	if gateOnLevel == "" || gateOnLevel == "none" {
		return nil
	}

	var gatingFailures []string

	for _, res := range sum.Results {
		// 건너뛰거나 차단된 결과는 무시함. 이는 Strictness 단위에서 처리됨.
		if res.Status == summary.StatusSkip || res.Status == summary.StatusBlock {
			continue
		}

		if gateOnLevel == "fail" && res.Status == summary.StatusFail {
			gatingFailures = append(gatingFailures, fmt.Sprintf("SLI %s failed: %s", res.ID, res.Reason))
		} else if gateOnLevel == "warn" && (res.Status == summary.StatusFail || res.Status == summary.StatusWarn) {
			gatingFailures = append(gatingFailures, fmt.Sprintf("SLI %s triggered level %s: %s", res.ID, res.Status, res.Reason))
		}
	}

	if len(gatingFailures) > 0 {
		return fmt.Errorf("GatingPolicy violation (%s): %s", gateOnLevel, strings.Join(gatingFailures, "; "))
	}

	return nil
}
