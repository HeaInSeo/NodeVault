// Package main is the NodeVault control plane entrypoint.
// Starts the gRPC server (PolicyService, BuildService, ValidateService, ToolRegistryService),
// the background reconcile loops, and the Harbor webhook HTTP server.
//
// The read-only Catalog REST HTTP server (NodePalette) runs as a separate binary:
// see cmd/palette/main.go.
package main

import (
	"context"
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
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
	"github.com/HeaInSeo/NodeVault/pkg/ping"
	"github.com/HeaInSeo/NodeVault/pkg/policy"
	"github.com/HeaInSeo/NodeVault/pkg/reconcile"
	"github.com/HeaInSeo/NodeVault/pkg/registry"
	"github.com/HeaInSeo/NodeVault/pkg/validate"
)

const (
	defaultGRPCAddr      = ":50051"
	defaultWebhookAddr   = ":8082"
	defaultFastReconcile = 5 * time.Minute
	defaultSlowReconcile = 30 * time.Minute
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
	buildBackend string // "local-podbridge" | "disabled"
	grpcAddr     string
	webhookAddr  string
	catalogPath  string
	indexPath    string
}

func loadRuntimeConfig() runtimeConfig {
	rc := runtimeConfig{
		runtimeMode:  os.Getenv("NODEVAULT_RUNTIME_MODE"),
		buildBackend: os.Getenv("NODEVAULT_BUILD_BACKEND"),
		grpcAddr:     sanitizeLogValue(os.Getenv("NODEVAULT_ADDR")),
		webhookAddr:  sanitizeLogValue(os.Getenv("NODEVAULT_WEBHOOK_ADDR")),
		catalogPath:  os.Getenv("CATALOG_DIR"),
		indexPath:    os.Getenv("INDEX_DIR"),
	}
	if rc.runtimeMode == "" {
		rc.runtimeMode = "host"
	}
	if rc.buildBackend == "" {
		rc.buildBackend = "local-podbridge"
	}
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
	return rc
}

func main() {
	// podbridge5 reexec is only needed for the local-podbridge build backend.
	// disabled and k8s-job modes do not initialize podbridge5 in-process.
	backend := os.Getenv("NODEVAULT_BUILD_BACKEND")
	if backend != "disabled" && backend != "k8s-job" {
		if podbridge5.ReexecIfNeeded() {
			os.Exit(0)
		}
	}
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	rc := loadRuntimeConfig()

	metrics.StartServer(os.Getenv("NODEVAULT_METRICS_ADDR"))

	// Log startup configuration for observability.
	kubeConfigMode := "kubeconfig_file"
	if rc.runtimeMode == "incluster" {
		kubeConfigMode = "incluster_serviceaccount"
	}
	slog.Info("NodeVault starting",
		"runtime_mode", rc.runtimeMode,
		"build_backend", rc.buildBackend,
		"catalog_path", rc.catalogPath,
		"index_path", rc.indexPath,
		"grpc_listen_address", rc.grpcAddr,
		"kube_config_mode", kubeConfigMode,
	)

	// Shared storage
	cat := catalog.NewCatalog()
	dataCat := catalog.NewDataCatalog()
	indexStore, indexErr := index.New()
	if indexErr != nil {
		slog.Error("failed to open index store", "err", indexErr)
		return 1
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

	// ValidateService — L3 dry-run + L4 smoke run.
	// In incluster mode use ServiceAccount token; in host mode use local kubeconfig.
	var validateSvc *validate.Service
	if rc.runtimeMode == "incluster" {
		validateSvc, err = validate.NewInClusterService()
	} else {
		validateSvc, err = validate.NewService()
	}
	if err != nil {
		slog.Warn("ValidateService unavailable", "runtime_mode", rc.runtimeMode, "err", err)
	} else {
		nfv1.RegisterValidateServiceServer(srv, validateSvc)
	}

	// ToolRegistryService — CAS storage + index dual-write (gRPC write path).
	registrySvc := catalog.NewToolRegistryService(cat, indexStore)
	nfv1.RegisterToolRegistryServiceServer(srv, registrySvc)

	// DataRegistryService — data artifact registration (gRPC write path).
	dataRegistrySvc := catalog.NewDataRegistryService(dataCat, indexStore)
	nfv1.RegisterDataRegistryServiceServer(srv, dataRegistrySvc)

	// Reconcile loops + webhook
	fastInterval := parseDuration("NODEVAULT_FAST_RECONCILE", defaultFastReconcile)
	slowInterval := parseDuration("NODEVAULT_SLOW_RECONCILE", defaultSlowReconcile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := startBackground(ctx, indexStore, rc.webhookAddr, fastInterval, slowInterval)

	registerBuildService(srv, &rc, validateSvc, registrySvc, indexStore, rec)

	//nolint:gosec // listener address is normalized before being attached to logs.
	slog.Info("NodeVault gRPC server starting", "addr", sanitizeLogValue(lis.Addr().String()))

	if serveErr := srv.Serve(lis); serveErr != nil {
		slog.Error("server exited", "err", serveErr)
		return 1
	}
	return 0
}

// registerBuildService selects and registers the BuildService backend based on NODEVAULT_BUILD_BACKEND.
// disabled: safe for K8s Pods (podbridge5 not initialized).
// k8s-job: spawns a privileged K8s Job per build request.
// default (local-podbridge): full podbridge5 in-process build (host mode only).
func registerBuildService(
	srv *grpc.Server,
	rc *runtimeConfig,
	validateSvc *validate.Service,
	registrySvc *catalog.ToolRegistryService,
	indexStore *index.Store,
	rec *reconcile.Reconciler,
) {
	switch rc.buildBackend {
	case "disabled":
		nfv1.RegisterBuildServiceServer(srv, build.NewDisabledService())
		slog.Info("BuildService registered with disabled backend (spike mode)")
	case "k8s-job":
		buildSvc, buildErr := build.NewK8sJobService(rc.runtimeMode, validateSvc, registrySvc, indexStore, rec)
		if buildErr != nil {
			slog.Warn("BuildService unavailable (k8s-job builder init failed)", "err", buildErr)
		} else {
			nfv1.RegisterBuildServiceServer(srv, buildSvc)
			slog.Info("BuildService registered with k8s-job backend (Option A spike)")
		}
	default:
		buildSvc, buildErr := build.NewService(validateSvc, registrySvc, indexStore, rec)
		if buildErr != nil {
			slog.Warn("BuildService unavailable (podbridge5 init failed?)", "err", buildErr)
		} else {
			nfv1.RegisterBuildServiceServer(srv, buildSvc)
		}
	}
}

// startBackground initializes the reconcile loops and the Harbor webhook HTTP server.
// Both run as background goroutines; ctx cancellation stops the reconcile loops.
// Returns the Reconciler so callers can trigger targeted reconciles (e.g. BuildService).
func startBackground(
	ctx context.Context, store *index.Store, webhookAddr string, fastInterval, slowInterval time.Duration,
) *reconcile.Reconciler {
	rec := reconcile.New(store, registry.NewHarborChecker())
	rec.RunFastLoop(ctx, fastInterval)
	rec.RunSlowLoop(ctx, slowInterval)
	slog.Info("reconcile loops started", "fast", fastInterval, "slow", slowInterval)

	webhookMux := http.NewServeMux()
	catalogrest.RegisterWebhook(webhookMux, store, rec)

	go func() {
		//nolint:gosec // webhookAddr is operator-configured (NODEVAULT_WEBHOOK_ADDR)
		slog.Info("NodeVault webhook server starting", "addr", webhookAddr)
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
	return rec
}

func sanitizeLogValue(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, v)
}
