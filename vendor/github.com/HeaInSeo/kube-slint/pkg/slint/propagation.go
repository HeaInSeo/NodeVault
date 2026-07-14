package slint

import (
	"fmt"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo/summary"
)

const modeBestEffort = "BestEffort"

// CheckStrictness evaluates the measurement pipeline's reliability based on
// StrictnessMode. Enforces the "Strictness" propagation contract.
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

	// In any non-BestEffort mode, an explicit block reason is always a failure.
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

// CheckGating evaluates successfully computed SLIs against the gating
// policy. Enforces the "GatingPolicy" propagation contract.
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
		// Skipped or blocked results are ignored here; they're handled by Strictness.
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
