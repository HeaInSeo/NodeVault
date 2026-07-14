package slint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/kubeutil"
	"github.com/HeaInSeo/kube-slint/pkg/slo/engine"
	"github.com/HeaInSeo/kube-slint/pkg/slo/fetch"
	"github.com/HeaInSeo/kube-slint/pkg/slo/spec"
	"github.com/HeaInSeo/kube-slint/pkg/slo/summary"
	"github.com/HeaInSeo/kube-slint/pkg/slo/tags"
)

// SessionConfig contains session inputs and defaults.
type SessionConfig struct {
	Namespace          string
	MetricsServiceName string
	TestCase           string
	Suite              string

	// Optional inputs
	RunID string
	Tags  map[string]string

	ServiceAccountName string
	Token              string
	ArtifactsDir       string

	Method engine.MeasurementMethod
	Now    func() time.Time

	// Optional overrides
	Specs   []spec.SLISpec
	Fetcher fetch.MetricsFetcher
	Writer  summary.Writer

	// Real-cluster Integration Knobs
	CurlImage string // overrides default curl image

	// TLSInsecureSkipVerify: true to skip self-signed cert verification (pass -k to curl).
	//
	// Deprecated: use DangerouslySkipTLSVerify instead — this field is kept
	// for backward compatibility and still takes effect (the two are OR'd).
	TLSInsecureSkipVerify bool

	// ServiceURLFormat overrides the default metrics URL template.
	// Format: two %s verbs — service name, namespace.
	// Default: "https://%s.%s.svc:8443/metrics"
	// Use "http://%s.%s.svc:8080/metrics" for plain-HTTP dev clusters.
	//
	// By default the resulting URL must resolve to a cluster-local Service
	// address ("<service>.<namespace>.svc[.cluster.local]"); anything else is
	// rejected before a curl pod is created. See
	// DangerouslyAllowExternalMetricsURL to opt out.
	ServiceURLFormat string

	// Security boundary bypasses — off by default. Each one allows behavior
	// that is otherwise rejected; see docs/security-model.md for the default
	// policy each of these overrides.
	DangerouslySkipTLSVerify            bool // disable TLS certificate verification (see TLSInsecureSkipVerify)
	DangerouslyAllowExternalMetricsURL  bool // allow ServiceURLFormat to resolve outside the cluster-local .svc boundary
	DangerouslyAllowKubeSystemNamespace bool // allow kube-system/kube-public/kube-node-lease as the target namespace

	// Internal metadata
	ConfigSourceType string
	ConfigSourcePath string

	StrictnessMode     string // BestEffort | StrictCollection | StrictEvaluation | RequiredSLIs
	MaxStartSkewMs     int64
	MaxEndSkewMs       int64
	MaxScrapeLatencyMs int64

	GateOnLevel    string // none | warn | fail
	CleanupEnabled bool
	CleanupMode    string // always | on-success | on-failure | manual
}

type sessionImpl struct {
	Config SessionConfig

	// Tunables (defaults are set in NewSession)
	ServiceURLFormat      string
	CurlImage             string
	TLSInsecureSkipVerify bool

	DangerouslySkipTLSVerify            bool
	DangerouslyAllowExternalMetricsURL  bool
	DangerouslyAllowKubeSystemNamespace bool

	ScrapeTimeout      time.Duration
	WaitPodDoneTimeout time.Duration
	LogsTimeout        time.Duration

	// Normalized (derived) runtime values
	RunID string
	Tags  map[string]string

	Warnings []string

	specs   []spec.SLISpec
	fetcher fetch.MetricsFetcher
	writer  summary.Writer

	// ownsFetcher is true when the session itself constructs the fetcher
	// (SessionConfig.Fetcher was nil). Only a session-owned fetcher's Stop()
	// is called from End() — a caller-supplied fetcher may be shared across
	// multiple sessions/scopes, and the caller is responsible for its lifecycle.
	ownsFetcher bool

	started   time.Time
	hasFailed bool
}

// Session manages a measurement session.
type Session struct {
	impl *sessionImpl
}

// NewSession builds a session with defaults applied.
func NewSession(cfg SessionConfig) *Session {
	cfg = applySessionBaseDefaults(cfg)
	cfg = discoverAndApplyConfig(cfg)

	autoTags := tags.AutoTags(tags.AutoTagsInput{
		Suite:     cfg.Suite,
		TestCase:  cfg.TestCase,
		Namespace: cfg.Namespace,
		RunID:     cfg.RunID,
	})

	mergedTags := tags.MergeTags(cfg.Tags, autoTags)
	resolvedSpecs := defaultSpecs(cfg.Specs)

	w := cfg.Writer
	if w == nil {
		w = summary.NewJSONFileWriter()
	}

	impl := &sessionImpl{
		Config: cfg,

		ServiceURLFormat: "https://%s.%s.svc:8443/metrics",
		CurlImage:        "docker.io/curlimages/curl:8.11.0",

		ScrapeTimeout:      2 * time.Minute,
		WaitPodDoneTimeout: 5 * time.Minute,
		LogsTimeout:        2 * time.Minute,

		RunID: cfg.RunID,
		Tags:  mergedTags,

		specs:       resolvedSpecs,
		fetcher:     cfg.Fetcher,
		ownsFetcher: cfg.Fetcher == nil,
		writer:      w,
	}

	if cfg.CurlImage != "" {
		impl.CurlImage = cfg.CurlImage
	}
	if cfg.ServiceURLFormat != "" {
		impl.ServiceURLFormat = cfg.ServiceURLFormat
	}
	impl.TLSInsecureSkipVerify = cfg.TLSInsecureSkipVerify
	impl.DangerouslySkipTLSVerify = cfg.DangerouslySkipTLSVerify
	impl.DangerouslyAllowExternalMetricsURL = cfg.DangerouslyAllowExternalMetricsURL
	impl.DangerouslyAllowKubeSystemNamespace = cfg.DangerouslyAllowKubeSystemNamespace

	return &Session{impl: impl}
}

func applySessionBaseDefaults(cfg SessionConfig) SessionConfig {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Method == "" {
		cfg.Method = engine.InsideSnapshot
	}

	runID := strings.TrimSpace(cfg.RunID)
	if runID == "" {
		runID = defaultRunID(cfg.Now())
	}
	cfg.RunID = runID
	return cfg
}

func defaultRunID(t time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("local-%d", t.UnixNano())
	}
	return fmt.Sprintf("local-%d-%s", t.UnixNano(), hex.EncodeToString(b[:]))
}

func discoverAndApplyConfig(cfg SessionConfig) SessionConfig {
	discoveredCfg, source, err := DiscoverConfig("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "kube-slint [discovery]: warning - %v\n", err)
	}

	cfg.ConfigSourceType = source.Type
	cfg.ConfigSourcePath = source.Path

	if source.Disabled {
		fmt.Fprintln(os.Stderr, "kube-slint [discovery]: discovery disabled via SLINT_DISABLE_DISCOVERY")
	} else {
		if source.Path != "" {
			fmt.Fprintf(os.Stderr, "kube-slint [discovery]: resolved config source type=%s path=%s\n", source.Type, source.Path)
		} else {
			fmt.Fprintf(os.Stderr, "kube-slint [discovery]: resolved config source type=%s\n", source.Type)
		}
	}

	if discoveredCfg != nil {
		cfg = mergeDiscoveredConfig(cfg, discoveredCfg)
	}
	return cfg
}

func mergeDiscoveredConfig(cfg SessionConfig, d *DiscoveredConfig) SessionConfig {
	if d.Write.ArtifactsDir != "" && cfg.ArtifactsDir == "" {
		cfg.ArtifactsDir = d.Write.ArtifactsDir
	}
	if d.Strictness.Mode != "" {
		cfg.StrictnessMode = d.Strictness.Mode
	}
	if d.Strictness.Thresholds.MaxStartSkewMs > 0 {
		cfg.MaxStartSkewMs = d.Strictness.Thresholds.MaxStartSkewMs
	}
	if d.Strictness.Thresholds.MaxEndSkewMs > 0 {
		cfg.MaxEndSkewMs = d.Strictness.Thresholds.MaxEndSkewMs
	}
	if d.Strictness.Thresholds.MaxScrapeLatencyMs > 0 {
		cfg.MaxScrapeLatencyMs = d.Strictness.Thresholds.MaxScrapeLatencyMs
	}
	if d.Gating.GateOnLevel != "" {
		cfg.GateOnLevel = d.Gating.GateOnLevel
	}
	cfg.CleanupEnabled = d.Cleanup.Enabled
	if d.Cleanup.Mode != "" {
		cfg.CleanupMode = d.Cleanup.Mode
	}
	return cfg
}

// reset swaps in from's internal runtime state without copying the whole
// Session struct. Note: this is not a deep copy, so from must not be used
// after reset is called.
func (s *Session) reset(from *Session) {
	if s == nil {
		return
	}

	if from == nil {
		s.impl = nil
		return
	}

	s.impl = from.impl
}

// ShouldWriteArtifacts reports whether the session should write summary output.
func (s *Session) ShouldWriteArtifacts() bool {
	// The s != nil check guards against panics from the Ginkgo hook +
	// placeholder-session pattern.
	return s != nil && s.impl != nil && s.impl.Config.ArtifactsDir != ""
}

// NextSummaryPath returns a unique summary path, appending -<n> on collision.
func (s *Session) NextSummaryPath(filename string) (string, error) {
	// The s == nil check is exceptionally needed given the current call pattern.
	if s == nil || s.impl == nil {
		return "", fmt.Errorf("slint: session not initialized")
	}

	if strings.TrimSpace(s.impl.Config.ArtifactsDir) == "" {
		return "", nil
	}

	base := filepath.Join(s.impl.Config.ArtifactsDir, filename)
	path := base
	for i := 1; ; i++ {
		_, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return path, nil
			}
			return "", err
		}
		path = fmt.Sprintf("%s-%d", base, i)
	}
}

// AddWarning records a warning message for BestEffort mode.
func (s *Session) AddWarning(message string) {
	if s == nil || s.impl == nil || strings.TrimSpace(message) == "" {
		return
	}
	s.impl.Warnings = append(s.impl.Warnings, message)
}

// MarkFailed explicitly flags the session as failed, affecting CleanupMode
// evaluation. It is monotonic (once marked failed, it cannot be undone) and
// idempotent.
func (s *Session) MarkFailed() {
	if s == nil || s.impl == nil {
		return
	}
	s.impl.hasFailed = true
}

// Cleanup removes temporary resources created by the session, using
// run-id and namespace as safeguards against overly broad deletes.
func (s *Session) Cleanup(ctx context.Context) {
	if s == nil || s.impl == nil {
		return
	}

	shouldRun := shouldRunCleanup(s.impl.Config.CleanupMode, s.impl.Config.CleanupEnabled, s.impl.hasFailed)
	if !shouldRun {
		return
	}

	ns := s.impl.Config.Namespace
	runID := s.impl.RunID

	if ns == "" || runID == "" {
		fmt.Fprintf(os.Stderr, "kube-slint [cleanup]: skip cleanup - missing namespace or run-id (ns=%q, runID=%q)\n", ns, runID)
		return
	}

	if kubeutil.IsDangerousNamespace(ns) && !s.impl.DangerouslyAllowKubeSystemNamespace {
		fmt.Fprintf(os.Stderr, "kube-slint [cleanup]: skip cleanup - namespace %q is cluster-critical and rejected by default; set DangerouslyAllowKubeSystemNamespace to override\n", ns)
		return
	}

	runCleanupActions(ctx, ns, runID)
}

func shouldRunCleanup(mode string, enabled, hasFailed bool) bool {
	if mode == "manual" {
		return false
	}

	if mode == "" {
		if enabled {
			mode = "always"
		} else {
			return false
		}
	}

	if mode == "on-success" && hasFailed {
		fmt.Fprintf(os.Stderr, "kube-slint [cleanup]: skip cleanup - mode is %s and test failed\n", mode)
		return false
	}
	if mode == "on-failure" && !hasFailed {
		fmt.Fprintf(os.Stderr, "kube-slint [cleanup]: skip cleanup - mode is %s and test succeeded\n", mode)
		return false
	}

	return true
}

func runCleanupActions(ctx context.Context, ns, runID string) {
	labelSelector := fmt.Sprintf("app.kubernetes.io/managed-by=kube-slint,slint-run-id=%s", SanitizeKubernetesLabelValue(runID))

	cmd := execCommandContext(ctx, "kubectl", "delete", "pods", "-n", ns, "-l", labelSelector, "--ignore-not-found=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kube-slint [cleanup]: failed for run-id %s: %v (output: %s)\n", runID, err, string(out))
	} else {
		outStr := strings.TrimSpace(string(out))
		if outStr != "" {
			fmt.Fprintf(os.Stderr, "kube-slint [cleanup]: success for run-id %s: %s\n", runID, outStr)
		}
	}
}

// Start begins measurement.
//
// If the fetcher implements fetch.SnapshotFetcher (e.g. curlPodFetcher), it
// pre-captures a start-time snapshot. This guarantees that when
// engine.Execute() calls Fetch() twice inside End(), the first call returns
// the pre-workload state.
//
// Note: when SnapshotFetcher.PreFetch() is implemented, Start() blocks for
// as long as the curl pod takes to run. On failure it prints a warning and
// continues (non-fatal, per kube-slint's safety-first principle).
func (s *Session) Start() {
	if s == nil || s.impl == nil {
		return
	}
	// Defensive: handle a nil or otherwise invalid injected Now().
	now := s.impl.Config.Now
	if now == nil {
		now = time.Now
	}

	t := now()
	if t.IsZero() {
		t = time.Now()
	}
	s.impl.started = t

	// If fetcher is nil (default curlpod path), construct it now so End()
	// shares the same instance — startCache lives on the instance, so it
	// must be the same one for PreFetch's result to reach Fetch().
	if s.impl.fetcher == nil {
		s.impl.fetcher = newCurlPodFetcher(s.impl)
	}

	// If the fetcher implements SnapshotFetcher, pre-capture a start
	// snapshot (closes Gap G). Fetchers that don't implement it (Mock,
	// httptest, etc.) are unaffected and behave as before.
	if sf, ok := s.impl.fetcher.(fetch.SnapshotFetcher); ok {
		ctx, cancel := context.WithTimeout(context.Background(), s.impl.podRunTimeout())
		defer cancel()
		if err := sf.PreFetch(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "kube-slint [prefetch]: warning - start snapshot failed: %v\n", err)
		}
	}
}

// podRunTimeout returns the outer deadline for a curl-pod-backed fetch
// (schedule + wait-until-terminal + log fetch). It must never be shorter than
// WaitPodDoneTimeout+LogsTimeout, otherwise those sub-timeouts are silently
// overridden by an earlier outer deadline (see docs/post-rc-hardening-design.md R4).
func (impl *sessionImpl) podRunTimeout() time.Duration {
	return impl.WaitPodDoneTimeout + impl.LogsTimeout + 30*time.Second
}

// End concludes the measurement session.
func (s *Session) End(ctx context.Context) (*summary.Summary, error) {
	if s == nil || s.impl == nil {
		return nil, fmt.Errorf("slint: session not initialized")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	started := s.impl.started
	if started.IsZero() {
		return nil, fmt.Errorf("slint: Start() was not called (RunID=%q TestCase=%q)", s.impl.RunID, s.impl.Config.TestCase)

	}

	// Important: time synchronization and injection timing deserve separate
	// discussion and documentation.
	now := s.impl.Config.Now
	if now == nil {
		now = time.Now
	}
	finished := now()

	m := s.impl.Config.Method
	if m == "" {
		m = engine.InsideSnapshot
	}

	fetcher := s.impl.fetcher
	if fetcher == nil {
		fetcher = newCurlPodFetcher(s.impl)
	}
	// Only stop a fetcher the session itself created. A caller-supplied
	// fetcher (SessionConfig.Fetcher) may be reused across multiple
	// sessions/scopes, and its lifecycle is the caller's responsibility.
	if s.impl.ownsFetcher {
		if sf, ok := fetcher.(interface{ Stop() }); ok {
			defer sf.Stop()
		}
	}

	eng := engine.New(fetcher, s.impl.writer, nil)

	// uniquePath: a unique file for the audit trail (never overwritten)
	// staticPath: the latest alias matching slint-gate's default input
	uniquePath := ""
	staticPath := ""
	if s.ShouldWriteArtifacts() {
		uniqueFilename := fmt.Sprintf(
			"sli-summary.%s.%s.json",
			SanitizeFilename(s.impl.RunID),
			SanitizeFilename(s.impl.Config.TestCase),
		)
		p, err := s.NextSummaryPath(uniqueFilename)
		if err != nil {
			return nil, err
		}
		uniquePath = p
		staticPath = filepath.Join(s.impl.Config.ArtifactsDir, "sli-summary.json")
	}

	rel := &summary.Reliability{
		ConfigSourceType: s.impl.Config.ConfigSourceType,
		ConfigSourcePath: s.impl.Config.ConfigSourcePath,
	}

	sum, err := engine.ExecuteStandard(ctx, eng, engine.ExecuteRequestStandard{
		Method: m,
		Config: engine.RunConfig{
			RunID:      s.impl.RunID,
			StartedAt:  started,
			FinishedAt: finished,
			Format:     "v4.4",
			Tags:       s.impl.Tags,
		},
		Specs:       s.impl.specs,
		OutPath:     uniquePath,
		Reliability: rel,
	})

	if err != nil {
		s.impl.hasFailed = true
		return sum, err
	}

	// static alias: kept aligned with slint-gate's default input path.
	// For parallel/multi-test runs, point --summary at uniquePath explicitly.
	if staticPath != "" && sum != nil {
		if writeErr := s.impl.writer.Write(staticPath, *sum); writeErr != nil {
			fmt.Fprintf(os.Stderr, "kube-slint [session]: warning - static alias write failed: %v\n", writeErr)
		}
	}

	// 1. Strictness check (pipeline reliability validation)
	if strictErr := CheckStrictness(s.impl.Config, sum); strictErr != nil {
		s.impl.hasFailed = true
		return sum, strictErr
	}

	// 2. Gating check (validates promotion of a healthy result)
	if gatingErr := CheckGating(s.impl.Config, sum); gatingErr != nil {
		s.impl.hasFailed = true
		return sum, gatingErr
	}

	return sum, nil
}

func defaultSpecs(specs []spec.SLISpec) []spec.SLISpec {
	if specs != nil {
		return specs
	}

	return DefaultV3Specs()
}
