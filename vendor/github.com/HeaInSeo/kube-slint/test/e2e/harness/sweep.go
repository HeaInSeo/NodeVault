package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	modeReportOnly = "report-only"
	modeDelete     = "delete"
)

var execCommandContext = exec.CommandContext

// OrphanSweepOptions 는 고아(orphan) 리소스 정리 동작을 설정함.
type OrphanSweepOptions struct {
	Enabled bool
	Mode    string        // "report-only" (기본값) | "delete"
	Limit   int           // 한 번에 삭제/보고할 최대 고아 리소스 수 (0이면 무제한)
	MaxAge  time.Duration // 이 시간보다 오래된 리소스만 대상으로 함 (0이면 검사 안 함)
}

// DevSweepOptions 제공: 로컬 개발 시 짧은 MaxAge와 적은 Limit
var DevSweepOptions = OrphanSweepOptions{
	Enabled: true,
	Mode:    modeReportOnly,
	Limit:   10,
	MaxAge:  10 * time.Minute,
}

// CISweepOptions 제공: CI 환경에서 적절한 Limit 부여
var CISweepOptions = OrphanSweepOptions{
	Enabled: true,
	Mode:    "report-only",
	Limit:   100,
	MaxAge:  1 * time.Hour,
}

// SweepRequest represents the request parameters for the orphan sweeper.
type SweepRequest struct {
	Namespace           string `json:"namespace"`
	Selector            string `json:"selector"`
	ModeRequested       string `json:"modeRequested"`
	Limit               int    `json:"limit"`
	MaxAgeSeconds       int    `json:"maxAgeSeconds"`
	ExcludeCurrentRunID bool   `json:"excludeCurrentRunId"`
	CurrentRunID        string `json:"currentRunId,omitempty"`
}

// SweepApply represents the applied mode and fallback status.
type SweepApply struct {
	ModeEffective  string `json:"modeEffective"`
	ModeFallback   bool   `json:"modeFallback"`
	FallbackReason string `json:"fallbackReason,omitempty"`
}

// SweepSummary provides aggregated counts of the sweep operation.
type SweepSummary struct {
	Scanned         int            `json:"scanned"`
	Evaluated       int            `json:"evaluated"`
	WouldDelete     int            `json:"wouldDelete"`
	Deleted         int            `json:"deleted"`
	DeleteError     int            `json:"deleteError"`
	Skipped         int            `json:"skipped"`
	SkippedByReason map[string]int `json:"skippedByReason"`
}

// SweepItem records the result for an individual orphan resource.
type SweepItem struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	CreatedAt  string `json:"createdAt,omitempty"`
	AgeSeconds int64  `json:"ageSeconds,omitempty"`
	RunID      string `json:"runId,omitempty"`
	Action     string `json:"action"` // "would-delete", "deleted", "skipped", "delete-error"
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SweepResult contains the complete output of an orphan sweep operation.
type SweepResult struct {
	SchemaVersion string       `json:"schemaVersion"`
	StartedAt     time.Time    `json:"startedAt"`
	FinishedAt    time.Time    `json:"finishedAt"`
	DurationMs    int64        `json:"durationMs"`
	Request       SweepRequest `json:"request"`
	Apply         SweepApply   `json:"apply"`
	Summary       SweepSummary `json:"summary"`
	Items         []SweepItem  `json:"items"`
	Warnings      []string     `json:"warnings,omitempty"`
}

// WriteSweepResultJSON writes the SweepResult to the provided io.Writer as pretty JSON.
func WriteSweepResultJSON(w io.Writer, r SweepResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// SweepOrphans 는 이전 kube-slint run-id의 리소스를 탐지하고 선택적으로 삭제함.
// 기존 API 호환성을 위해 SweepOrphansWithResult 결과를 버리고 성공 여부만 반환함.
func (s *Session) SweepOrphans(ctx context.Context, opts OrphanSweepOptions) error {
	_, err := s.SweepOrphansWithResult(ctx, opts)
	return err
}

// SweepOrphansWithResult detects and optionally deletes resources from previous kube-slint run-ids.
// It returns a SweepResult struct containing the detailed outcome of the sweep operation.
func (s *Session) SweepOrphansWithResult(ctx context.Context, opts OrphanSweepOptions) (SweepResult, error) {
	startedAt := time.Now()
	res := initSweepResult(opts, startedAt)

	if s == nil || s.impl == nil || !opts.Enabled {
		finalizeSweepResult(&res)
		return res, nil
	}

	ns := s.impl.Config.Namespace
	runID := s.impl.RunID
	if ns == "" || runID == "" {
		appendSkipGuard(&res)
		finalizeSweepResult(&res)
		return res, nil
	}

	res.Request.Namespace = ns
	res.Request.CurrentRunID = runID
	res.Request.ExcludeCurrentRunID = true
	res.Request.ModeRequested = opts.Mode
	normalizeSweepMode(opts.Mode, &res)

	labelSelector := fmt.Sprintf("app.kubernetes.io/managed-by=kube-slint,slint-run-id!=%s", runID)
	res.Request.Selector = labelSelector

	lines, err := listOrphanCandidates(ctx, ns, labelSelector)
	if err != nil {
		finalizeSweepResult(&res)
		return res, err
	}
	if len(lines) == 0 {
		fmt.Printf("kube-slint [orphan-sweep]: mode=%s ns=%s run-id=%s :: no orphan resources detected\n",
			res.Apply.ModeEffective, ns, runID)
		finalizeSweepResult(&res)
		return res, nil
	}

	var targetNames []string
	var hitLimit int
	for _, line := range lines {
		evaluateSweepCandidate(line, opts, ns, startedAt, &res, &targetNames, &hitLimit)
	}

	printSweepSummary(&res, ns, runID, opts, targetNames, hitLimit)

	err = applySweepDeletes(ctx, ns, targetNames, &res)

	finalizeSweepResult(&res)
	return res, err
}

func initSweepResult(opts OrphanSweepOptions, startedAt time.Time) SweepResult {
	res := SweepResult{
		SchemaVersion: "v1.0",
		StartedAt:     startedAt,
		Summary: SweepSummary{
			SkippedByReason: make(map[string]int),
		},
		Items:    []SweepItem{},
		Warnings: []string{},
	}
	res.Request.Limit = opts.Limit
	res.Request.MaxAgeSeconds = int(opts.MaxAge.Seconds())
	return res
}

func finalizeSweepResult(res *SweepResult) {
	res.FinishedAt = time.Now()
	res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
}

func appendSkipGuard(res *SweepResult) {
	res.Summary.Skipped++
	res.Summary.SkippedByReason["missing_guard"]++
	warnMsg := "skip - missing namespace or run-id for safety guard"
	res.Warnings = append(res.Warnings, warnMsg)
	fmt.Printf("kube-slint [orphan-sweep]: %s\n", warnMsg)
}

func normalizeSweepMode(modeReq string, res *SweepResult) {
	modeReq = strings.TrimSpace(modeReq)
	modeEff := modeReq
	fallback := false
	fallbackReason := ""

	if modeEff == "" {
		modeEff = modeReportOnly
	} else if modeEff != modeDelete && modeEff != modeReportOnly {
		modeEff = modeReportOnly
		fallback = true
		fallbackReason = "invalid_mode"
		warnMsg := fmt.Sprintf("invalid mode %q provided, falling back to report-only", modeReq)
		res.Warnings = append(res.Warnings, warnMsg)
		fmt.Printf("kube-slint [orphan-sweep]: warning - %s\n", warnMsg)
	}

	res.Apply.ModeEffective = modeEff
	res.Apply.ModeFallback = fallback
	res.Apply.FallbackReason = fallbackReason
}

func listOrphanCandidates(ctx context.Context, ns, selector string) ([]string, error) {
	jp := "jsonpath={range .items[*]}{.metadata.name}" +
		",{.metadata.labels.slint-run-id},{.metadata.creationTimestamp}{\"\\n\"}{end}"
	cmd := execCommandContext(ctx, "kubectl", "get", "pods", "-n", ns, "-l", selector, "-o", jp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list orphans: %v (output: %s)", err, string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil // empty
	}
	return lines, nil
}

func parseTimestamp(tsStr string) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
		return ts, nil
	}
	return time.Parse(time.RFC3339, tsStr)
}

func evaluateSweepCandidate(
	line string, opts OrphanSweepOptions, ns string, startedAt time.Time,
	res *SweepResult, targetNames *[]string, hitLimit *int) {
	parts := strings.SplitN(line, ",", 3)
	if len(parts) != 3 {
		return
	}
	res.Summary.Scanned++
	name, rId, tsStr := parts[0], parts[1], parts[2]

	item := SweepItem{Kind: "Pod", Namespace: ns, Name: name, RunID: rId, CreatedAt: tsStr}

	ts, err := parseTimestamp(tsStr)
	if err != nil {
		warnMsg := fmt.Sprintf("failed to parse creation timestamp for pod %s: %v", name, err)
		res.Warnings = append(res.Warnings, warnMsg)
		fmt.Printf("kube-slint [orphan-sweep]: warning - %s\n", warnMsg)
		if opts.MaxAge > 0 {
			appendSkipReason(res, &item, "timestamp_parse_failed")
			return
		}
	} else {
		item.AgeSeconds = int64(startedAt.Sub(ts).Seconds())
		if opts.MaxAge > 0 && startedAt.Sub(ts) < opts.MaxAge {
			appendSkipReason(res, &item, "max_age_not_reached")
			return
		}
	}

	res.Summary.Evaluated++

	if opts.Limit > 0 && len(*targetNames) >= opts.Limit {
		appendSkipReason(res, &item, "limit_exceeded")
		*hitLimit++
		return
	}

	*targetNames = append(*targetNames, name)
	item.Action = "would-delete"
	item.Reason = "matched"
	if res.Apply.ModeEffective != modeDelete {
		res.Summary.WouldDelete++
	}
	res.Items = append(res.Items, item)
}

func appendSkipReason(res *SweepResult, item *SweepItem, reason string) {
	item.Action = "skipped"
	item.Reason = reason
	res.Summary.Skipped++
	res.Summary.SkippedByReason[reason]++
	res.Items = append(res.Items, *item)
}

func printSweepSummary(
	res *SweepResult, ns, runID string, opts OrphanSweepOptions, targetNames []string, hitLimit int) {
	fmt.Printf("kube-slint [orphan-sweep]: mode=%s ns=%s run-id=%s limit=%d maxAge=%v\n",
		res.Apply.ModeEffective, ns, runID, opts.Limit, opts.MaxAge)
	fmt.Printf("kube-slint [orphan-sweep]: detected %d matching orphan(s) ", res.Summary.Evaluated)
	if hitLimit > 0 {
		fmt.Printf("(processing %d, skipping %d due to limit)\n", len(targetNames), hitLimit)
	} else {
		fmt.Printf("(processing all)\n")
	}
}

func applySweepDeletes(ctx context.Context, ns string, targetNames []string, res *SweepResult) error {
	if res.Apply.ModeEffective != modeDelete {
		if len(targetNames) > 0 {
			fmt.Printf("kube-slint [orphan-sweep]: report-only mode, skipped deletion of %v\n", targetNames)
			fmt.Printf("kube-slint [orphan-sweep]: to delete, set option mode='delete'\n")
		}
		return nil
	}
	if len(targetNames) == 0 {
		fmt.Printf("kube-slint [orphan-sweep]: no targets to delete\n")
		return nil
	}

	fmt.Printf("kube-slint [orphan-sweep]: proceeding with individual deletion for %d orphan(s)...\n", len(targetNames))
	var hasError bool

	itemIdx := make(map[string]int)
	for i, it := range res.Items {
		itemIdx[it.Name] = i
	}

	for _, name := range targetNames {
		cmd := execCommandContext(ctx, "kubectl", "delete", "pods", name, "-n", ns, "--ignore-not-found=true")
		delOut, delErr := cmd.CombinedOutput()
		idx, ok := itemIdx[name]
		if !ok {
			continue // Should not happen
		}

		if delErr != nil {
			hasError = true
			res.Warnings = append(res.Warnings, fmt.Sprintf("delete failed for %s: %v", name, delErr))
			res.Items[idx].Action = "delete-error"
			res.Items[idx].Error = fmt.Sprintf("kubectl fail: %v (out: %s)", delErr, strings.TrimSpace(string(delOut)))
			res.Summary.DeleteError++
		} else {
			res.Items[idx].Action = "deleted"
			res.Summary.Deleted++
		}
	}

	if hasError {
		return fmt.Errorf("some orphan deletions failed, check result for details")
	}
	fmt.Printf("kube-slint [orphan-sweep]: deletion complete\n")
	return nil
}
