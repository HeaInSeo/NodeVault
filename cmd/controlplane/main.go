// Package main is the NodeVault control plane entrypoint.
// Starts the gRPC server (PolicyService, BuildService, ValidateService, ToolRegistryService),
// the background reconcile loops, and the Harbor webhook HTTP server.
//
// The read-only Catalog REST HTTP server (NodePalette) runs as a separate binary:
// see cmd/palette/main.go.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/podbridge5"

	"github.com/HeaInSeo/NodeVault/pkg/build"
	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/cachegc"
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/certification"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
	"github.com/HeaInSeo/NodeVault/pkg/ping"
	"github.com/HeaInSeo/NodeVault/pkg/policy"
	"github.com/HeaInSeo/NodeVault/pkg/reconcile"
	"github.com/HeaInSeo/NodeVault/pkg/registry"
	"github.com/HeaInSeo/NodeVault/pkg/registryconfig"
	"github.com/HeaInSeo/NodeVault/pkg/validate"
	"github.com/HeaInSeo/NodeVault/pkg/validation"
)

const (
	defaultGRPCAddr      = ":50051"
	defaultWebhookAddr   = ":8082"
	defaultBuildStateDB  = "assets/buildstate/build-state.db"
	defaultFastReconcile = 5 * time.Minute
	defaultSlowReconcile = 30 * time.Minute

	buildBackendInPodBuildah      = "in-pod-buildah"
	buildBackendDisabled          = "disabled"
	legacyBuildBackendPodbridge   = "local-podbridge"
	removedBuildBackendKubernetes = "k8s-job"
)

// parseDuration reads a duration from an env var, falling back to def on parse error or absence.
func parseDuration(env string, def time.Duration) time.Duration {
	if s := os.Getenv(env); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		slog.Warn("invalid duration env var — using default", "env", env, "default", def)
	}
	return def
}

// runtimeConfig holds resolved startup configuration derived from env vars.
type runtimeConfig struct {
	runtimeMode  string // "host" | "incluster"
	buildBackend string // "in-pod-buildah" | "disabled"
	grpcAddr     string
	webhookAddr  string
	catalogPath  string
	indexPath    string
	buildStateDB string
}

func loadRuntimeConfig() (runtimeConfig, error) {
	rc := runtimeConfig{
		runtimeMode:  os.Getenv("NODEVAULT_RUNTIME_MODE"),
		buildBackend: os.Getenv("NODEVAULT_BUILD_BACKEND"),
		grpcAddr:     sanitizeLogValue(os.Getenv("NODEVAULT_ADDR")),
		webhookAddr:  sanitizeLogValue(os.Getenv("NODEVAULT_WEBHOOK_ADDR")),
		catalogPath:  os.Getenv("CATALOG_DIR"),
		indexPath:    os.Getenv("INDEX_DIR"),
		buildStateDB: os.Getenv("NODEVAULT_BUILD_STATE_DB"),
	}
	if rc.runtimeMode == "" {
		rc.runtimeMode = "host"
	}

	backend, err := normalizeBuildBackend(rc.buildBackend)
	if err != nil {
		return runtimeConfig{}, err
	}
	rc.buildBackend = backend

	if rc.grpcAddr == "" {
		rc.grpcAddr = defaultGRPCAddr
	}
	if rc.webhookAddr == "" {
		rc.webhookAddr = defaultWebhookAddr
	}
	if rc.catalogPath == "" {
		rc.catalogPath = "assets/catalog"
	}
	if rc.indexPath == "" {
		rc.indexPath = "assets/index"
	}
	if rc.buildStateDB == "" {
		rc.buildStateDB = defaultBuildStateDB
	}
	return rc, nil
}

func normalizeBuildBackend(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", buildBackendInPodBuildah:
		return buildBackendInPodBuildah, nil
	case legacyBuildBackendPodbridge:
		// Compatibility for existing host deployments. The implementation is the
		// same in-process podbridge5/Buildah path and is no longer host-only.
		return buildBackendInPodBuildah, nil
	case buildBackendDisabled:
		return buildBackendDisabled, nil
	case removedBuildBackendKubernetes:
		return "", fmt.Errorf(
			"NODEVAULT_BUILD_BACKEND=%q was removed: "+
				"NodeVault must build in its own Pod through podbridge5/Buildah, not create a builder Job",
			removedBuildBackendKubernetes,
		)
	default:
		return "", fmt.Errorf(
			"unsupported NODEVAULT_BUILD_BACKEND %q (supported: %q, %q)",
			raw, buildBackendInPodBuildah, buildBackendDisabled,
		)
	}
}

func main() {
	// podbridge5/Buildah reexec is part of the in-process image build path.
	backend, err := normalizeBuildBackend(os.Getenv("NODEVAULT_BUILD_BACKEND"))
	if err == nil && backend == buildBackendInPodBuildah {
		if podbridge5.ReexecIfNeeded() {
			os.Exit(0)
		}
	}
	os.Exit(run())
}

//nolint:funlen // startup orchestration — linear sequence of independent service registrations.
func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	rc, configErr := loadRuntimeConfig()
	if configErr != nil {
		slog.Error("invalid NodeVault runtime configuration", "err", configErr)
		return 1
	}
	if os.Getenv("NODEVAULT_BUILD_BACKEND") == legacyBuildBackendPodbridge {
		slog.Warn(
			"NODEVAULT_BUILD_BACKEND=local-podbridge is deprecated; use in-pod-buildah",
		)
	}

	metrics.StartServer(os.Getenv("NODEVAULT_METRICS_ADDR"))
	logStartupConfig(rc)

	// Shared storage
	cat := catalog.NewCatalog()
	dataCat := catalog.NewDataCatalog()
	indexStore, indexErr := index.New()
	if indexErr != nil {
		slog.Error("failed to open index store", "err", indexErr)
		return 1
	}
	buildStateStore, buildStateErr := buildstate.Open(rc.buildStateDB)
	if buildStateErr != nil {
		slog.Error("failed to open build state store", "err", buildStateErr)
		return 1
	}
	defer func() {
		if closeErr := buildStateStore.Close(); closeErr != nil {
			slog.Warn("failed to close build state store", "err", closeErr)
		}
	}()
	if recovered, recoverErr := buildStateStore.RecoverInterrupted(time.Now().UTC()); recoverErr != nil {
		slog.Error("failed to recover interrupted builds", "err", recoverErr)
		return 1
	} else if recovered > 0 {
		slog.Warn("recovered interrupted builds", "count", recovered)
	}

	// gRPC server
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", rc.grpcAddr)
	if err != nil {
		//nolint:gosec // rc.grpcAddr is normalized to a single-line value before logging.
		slog.Error("failed to listen", "addr", rc.grpcAddr, "err", err)
		return 1
	}

	srv := grpc.NewServer()

	// PingService — Phase 0 connectivity check.
	nfv1.RegisterPingServiceServer(srv, ping.NewHandler())

	// PolicyService — serves dockguard.wasm bundle to NodeKit.
	nfv1.RegisterPolicyServiceServer(srv, policy.NewService())

	initValidateService(srv, &rc)
	registrySvc := registerCatalogServices(srv, cat, dataCat, indexStore)

	// Reconcile loops + webhook + validation REST
	fastInterval := parseDuration("NODEVAULT_FAST_RECONCILE", defaultFastReconcile)
	slowInterval := parseDuration("NODEVAULT_SLOW_RECONCILE", defaultSlowReconcile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Certification service — shared between gRPC and REST validation paths.
	certSvc := certification.New(indexStore)

	rec, err := startBackground(ctx, indexStore, cat, dataCat, certSvc, rc.webhookAddr, fastInterval, slowInterval)
	if err != nil {
		slog.Error("failed to start background services", "err", err)
		return 1
	}

	// Package cache GC — evicts oldest conda/mamba packages when PVC usage exceeds watermark.
	go cachegc.New(cachegc.DefaultConfig()).Run(ctx)

	if registerErr := registerBuildService(
		srv, &rc, registrySvc, indexStore, buildStateStore, rec,
	); registerErr != nil {
		slog.Error("failed to register BuildService", "err", registerErr)
		return 1
	}

	// ValidationResultService — receives L5-a/L5-b results from NodeSentinel via gRPC.
	nfv1.RegisterValidationResultServiceServer(srv, validation.New(indexStore, certSvc))

	//nolint:gosec // listener address is normalized before being attached to logs.
	slog.Info("NodeVault gRPC server starting", "addr", sanitizeLogValue(lis.Addr().String()))

	if serveErr := srv.Serve(lis); serveErr != nil {
		slog.Error("server exited", "err", serveErr)
		return 1
	}
	return 0
}

// initValidateService creates the ValidateService for the given runtime mode and registers it
// on srv. Returns nil (and logs a warning) if the kubeconfig is unavailable — other services
// continue to operate normally in that case.
func initValidateService(srv *grpc.Server, rc *runtimeConfig) *validate.Service {
	var svc *validate.Service
	var err error
	if rc.runtimeMode == "incluster" {
		svc, err = validate.NewInClusterService()
	} else {
		svc, err = validate.NewService()
	}
	if err != nil {
		slog.Warn("ValidateService unavailable", "runtime_mode", rc.runtimeMode, "err", err)
		return nil
	}
	nfv1.RegisterValidateServiceServer(srv, svc)
	return svc
}

// registerCatalogServices registers ToolRegistryService and DataRegistryService on srv.
// Returns the ToolRegistryService so the caller can pass it to BuildService.
func registerCatalogServices(
	srv *grpc.Server,
	cat *catalog.Catalog,
	dataCat *catalog.DataCatalog,
	indexStore *index.Store,
) *catalog.ToolRegistryService {
	registrySvc := catalog.NewToolRegistryService(cat, indexStore)
	nfv1.RegisterToolRegistryServiceServer(srv, registrySvc)
	dataRegistrySvc := catalog.NewDataRegistryService(dataCat, indexStore)
	nfv1.RegisterDataRegistryServiceServer(srv, dataRegistrySvc)
	return registrySvc
}

// logStartupConfig emits a structured log line with the active runtime configuration.
//
//nolint:gocritic // hugeParam: runtimeConfig by value is intentional — read-only helper.
func logStartupConfig(rc runtimeConfig) {
	kubeConfigMode := "kubeconfig_file"
	if rc.runtimeMode == "incluster" {
		kubeConfigMode = "incluster_serviceaccount"
	}
	slog.Info("NodeVault starting",
		"runtime_mode", rc.runtimeMode,
		"build_backend", rc.buildBackend,
		"catalog_path", rc.catalogPath,
		"index_path", rc.indexPath,
		"build_state_db", rc.buildStateDB,
		"grpc_listen_address", rc.grpcAddr,
		"kube_config_mode", kubeConfigMode,
	)
}

// registerBuildService registers the single production image build path.
// in-pod-buildah: NodeVault calls podbridge5, which uses the Buildah Go API in
// the same NodeVault process/Pod. No builder Job or worker Pod is created.
// disabled: keeps non-build APIs available while explicitly rejecting builds.
func registerBuildService(
	srv *grpc.Server,
	rc *runtimeConfig,
	registrySvc *catalog.ToolRegistryService,
	indexStore *index.Store,
	buildStateStore *buildstate.Store,
	rec *reconcile.Reconciler,
) error {
	switch rc.buildBackend {
	case buildBackendDisabled:
		nfv1.RegisterBuildServiceServer(srv, build.NewDisabledService())
		slog.Info("BuildService registered with disabled backend")
		return nil
	case buildBackendInPodBuildah:
		buildSvc, buildErr := build.NewService(registrySvc, indexStore, buildStateStore, rec)
		if buildErr != nil {
			return fmt.Errorf("initialize in-pod podbridge5/Buildah builder: %w", buildErr)
		}
		nfv1.RegisterBuildServiceServer(srv, buildSvc)
		slog.Info("BuildService registered", "backend", buildBackendInPodBuildah)
		return nil
	default:
		return fmt.Errorf("internal error: unnormalized build backend %q", rc.buildBackend)
	}
}

// startBackground initializes the reconcile loops and the combined webhook+validation
// HTTP server. Both run as background goroutines; ctx cancellation stops the reconcile loops.
// Returns the Reconciler so callers can trigger targeted reconciles (e.g. BuildService).
func startBackground(
	ctx context.Context,
	store *index.Store,
	cat *catalog.Catalog,
	dataCat *catalog.DataCatalog,
	certSvc *certification.Service,
	webhookAddr string,
	fastInterval, slowInterval time.Duration,
) (*reconcile.Reconciler, error) {
	checker, err := registry.NewHarborChecker(registryconfig.FromEnv())
	if err != nil {
		return nil, fmt.Errorf("build harbor checker: %w", err)
	}
	rec := reconcile.New(store, checker)
	rec.RunFastLoop(ctx, fastInterval)
	rec.RunSlowLoop(ctx, slowInterval)
	slog.Info("reconcile loops started", "fast", fastInterval, "slow", slowInterval)

	// Single HTTP server: Harbor webhook + validation REST POST endpoints.
	webhookMux := catalogrest.NewMuxWithCert(store, cat, dataCat, certSvc)
	catalogrest.RegisterWebhook(webhookMux, store, rec)

	go func() {
		//nolint:gosec // webhookAddr is operator-configured (NODEVAULT_WEBHOOK_ADDR)
		slog.Info("NodeVault webhook+validation server starting", "addr", webhookAddr)
		webhookSrv := &http.Server{
			Addr:         webhookAddr,
			Handler:      webhookMux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		if err := webhookSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("webhook server exited", "err", err)
		}
	}()
	return rec, nil
}

func sanitizeLogValue(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, v)
}
