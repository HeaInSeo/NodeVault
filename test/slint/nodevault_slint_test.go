//go:build slint

// Package slintgate contains kube-slint SLI gate tests for NodeVault.
//
// Purpose: suppress K8s churn and detect reconcile regression in CI.
// The test starts a NodeVault binary in disabled-build mode, runs a
// kube-slint measurement window, and enforces churn + error thresholds.
//
// Usage:
//
//	go test -tags slint -timeout 120s -v ./test/slint/...
//	NODEVAULT_BIN=./bin/nodevault go test -tags slint -timeout 120s -v ./test/slint/...
package slintgate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/slint"
	"github.com/HeaInSeo/kube-slint/pkg/slo/fetch"
	"github.com/HeaInSeo/kube-slint/pkg/slo/spec"
	"github.com/HeaInSeo/kube-slint/pkg/slo/summary"
)

const (
	testMetricsAddr   = "127.0.0.1:19090"
	testGRPCAddr      = "127.0.0.1:15051"
	testWebhookAddr   = "127.0.0.1:18082"
	testFastReconcile = "5s"
	testSlowReconcile = "1h" // suppress slow loop in test window
	observationWindow = 25 * time.Second
	readinessTimeout  = 15 * time.Second
)

// TestNodeVaultSlintGate measures NodeVault's reconcile loop SLI over a 25-second window.
//
// Thresholds (churn gate):
//   - reconcile_fast_delta >= 1   (loop ran at least once — liveness check)
//   - reconcile_fast_delta <= 15  (no churn — at most ~5 ticks in 25s window)
//   - reconcile_error_delta == 0  (no reconcile errors — regression check)
//   - reconcile_slow_delta == 0   (testSlowReconcile=1h never fires in a 25s window)
//   - build_failure_delta == 0    (NODEVAULT_BUILD_BACKEND=disabled: no build is
//     submitted during this window, so the failure counter must not move)
func TestNodeVaultSlintGate(t *testing.T) {
	binPath := os.Getenv("NODEVAULT_BIN")
	if binPath == "" {
		binPath = "../../bin/nodevault"
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("nodevault binary not found at %s (set NODEVAULT_BIN): %v", binPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Start NodeVault with build disabled and fast reconcile at 5s.
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(os.Environ(),
		"NODEVAULT_BUILD_BACKEND=disabled",
		"NODEVAULT_FAST_RECONCILE="+testFastReconcile,
		"NODEVAULT_SLOW_RECONCILE="+testSlowReconcile,
		"NODEVAULT_METRICS_ADDR="+testMetricsAddr,
		"NODEVAULT_ADDR="+testGRPCAddr,
		"NODEVAULT_WEBHOOK_ADDR="+testWebhookAddr,
		"CATALOG_DIR="+t.TempDir(),
		"INDEX_DIR="+t.TempDir(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nodevault: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	if err := waitForHealthz(testMetricsAddr, readinessTimeout); err != nil {
		t.Fatalf("nodevault metrics not ready: %v", err)
	}

	fetcher := newExpvarFetcher(testMetricsAddr)
	sess := slint.NewSession(slint.SessionConfig{
		TestCase:     "nodevault-churn-gate",
		ArtifactsDir: t.TempDir(),
		Fetcher:      fetcher,
		Specs:        nodevaultSpecs(),
	})
	sess.Start()
	time.Sleep(observationWindow)

	sum, err := sess.End(ctx)
	if err != nil {
		t.Fatalf("session end: %v", err)
	}
	if sum == nil {
		t.Fatal("nil summary returned")
	}

	results := indexResults(sum.Results)
	assertGE(t, results, "reconcile_fast_delta", 1)
	assertLE(t, results, "reconcile_fast_delta", 15)
	assertEQ(t, results, "reconcile_error_delta", 0)
	assertEQ(t, results, "reconcile_slow_delta", 0)
	assertEQ(t, results, "build_failure_delta", 0)
}

// nodevaultSpecs defines the SLI spec set for NodeVault's reconcile counters.
func nodevaultSpecs() []spec.SLISpec {
	return []spec.SLISpec{
		{
			ID:      "reconcile_fast_delta",
			Title:   "Reconcile Fast Loop Iterations",
			Unit:    "count",
			Kind:    "delta_counter",
			Inputs:  []spec.MetricRef{spec.UnsafePromKey("nodevault_reconcile_fast_total")},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:      "reconcile_slow_delta",
			Title:   "Reconcile Slow Loop Iterations",
			Unit:    "count",
			Kind:    "delta_counter",
			Inputs:  []spec.MetricRef{spec.UnsafePromKey("nodevault_reconcile_slow_total")},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:      "reconcile_error_delta",
			Title:   "Reconcile Loop Errors",
			Unit:    "count",
			Kind:    "delta_counter",
			Inputs:  []spec.MetricRef{spec.UnsafePromKey("nodevault_reconcile_error_total")},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
		{
			ID:      "build_failure_delta",
			Title:   "Build Failures",
			Unit:    "count",
			Kind:    "delta_counter",
			Inputs:  []spec.MetricRef{spec.UnsafePromKey("nodevault_build_failure_total")},
			Compute: spec.ComputeSpec{Mode: spec.ComputeDelta},
		},
	}
}

// ── expvar fetcher ────────────────────────────────────────────────────────────

// expvarFetcher implements fetch.SnapshotFetcher by scraping /debug/vars HTTP JSON.
// PreFetch caches the start snapshot so engine.Execute() gets an accurate delta.
type expvarFetcher struct {
	url         string
	mu          sync.Mutex
	startSample *fetch.Sample
}

func newExpvarFetcher(addr string) *expvarFetcher {
	return &expvarFetcher{url: "http://" + addr + "/debug/vars"}
}

// PreFetch captures the start-of-window snapshot (called by Session.Start).
func (f *expvarFetcher) PreFetch(ctx context.Context) error {
	s, err := f.scrape(ctx)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.startSample = &s
	f.mu.Unlock()
	return nil
}

// Fetch returns the cached start snapshot on first call, then a fresh scrape.
func (f *expvarFetcher) Fetch(ctx context.Context, _ time.Time) (fetch.Sample, error) {
	f.mu.Lock()
	cached := f.startSample
	if cached != nil {
		f.startSample = nil
		f.mu.Unlock()
		return *cached, nil
	}
	f.mu.Unlock()
	return f.scrape(ctx)
}

func (f *expvarFetcher) scrape(ctx context.Context) (fetch.Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, http.NoBody)
	if err != nil {
		return fetch.Sample{}, fmt.Errorf("expvar fetch: new request: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fetch.Sample{}, fmt.Errorf("expvar fetch: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fetch.Sample{}, fmt.Errorf("expvar fetch: status %d", resp.StatusCode)
	}
	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return fetch.Sample{}, fmt.Errorf("expvar fetch: decode: %w", err)
	}
	values := make(map[string]float64, len(raw))
	for k, v := range raw {
		if n, ok := v.(float64); ok {
			values[k] = n
		}
	}
	return fetch.Sample{At: time.Now(), Values: values}, nil
}

// ── assertion helpers ─────────────────────────────────────────────────────────

func indexResults(results []summary.SLIResult) map[string]float64 {
	m := make(map[string]float64, len(results))
	for _, r := range results {
		if r.Value != nil {
			m[r.ID] = *r.Value
		}
	}
	return m
}

func assertGE(t *testing.T, results map[string]float64, id string, threshold float64) {
	t.Helper()
	v, ok := results[id]
	if !ok {
		t.Errorf("SLI %q not found in summary", id)
		return
	}
	if v < threshold {
		t.Errorf("SLI %q = %.0f, want >= %.0f", id, v, threshold)
	}
}

func assertLE(t *testing.T, results map[string]float64, id string, threshold float64) {
	t.Helper()
	v, ok := results[id]
	if !ok {
		t.Errorf("SLI %q not found in summary", id)
		return
	}
	if v > threshold {
		t.Errorf("SLI %q = %.0f, want <= %.0f (churn detected)", id, v, threshold)
	}
}

func assertEQ(t *testing.T, results map[string]float64, id string, expected float64) {
	t.Helper()
	v, ok := results[id]
	if !ok {
		t.Errorf("SLI %q not found in summary", id)
		return
	}
	if v != expected {
		t.Errorf("SLI %q = %.0f, want %.0f", id, v, expected)
	}
}

// ── readiness polling ─────────────────────────────────────────────────────────

func waitForHealthz(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	//nolint:gosec // G107: addr is a test-internal constant (127.0.0.1:port), not external input
	healthzURL := "http://" + addr + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		pollCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, healthzURL, http.NoBody)
		if err != nil {
			cancel()
			return fmt.Errorf("waitForHealthz: build request: %w", err)
		}
		resp, err := client.Do(req)
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("healthz at %s not ready after %s", addr, timeout)
}
