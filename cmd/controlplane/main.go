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

func main() {
	// Required before storage/build initialization in podbridge5 rootless mode.
	if podbridge5.ReexecIfNeeded() {
		os.Exit(0)
	}
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	grpcAddr := os.Getenv("NODEVAULT_ADDR")
	if grpcAddr == "" {
		grpcAddr = defaultGRPCAddr
	}
	grpcAddr = sanitizeLogValue(grpcAddr)

	webhookAddr := os.Getenv("NODEVAULT_WEBHOOK_ADDR")
	if webhookAddr == "" {
		webhookAddr = defaultWebhookAddr
	}
	webhookAddr = sanitizeLogValue(webhookAddr)

	// ── Shared storage ──────────────────────────────────────────────────────

	cat := catalog.NewCatalog()
	dataCat := catalog.NewDataCatalog()
	indexStore, indexErr := index.New()
	if indexErr != nil {
		slog.Error("failed to open index store", "err", indexErr)
		return 1
	}

	// ── gRPC server ──────────────────────────────────────────────────────────

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", grpcAddr)
	if err != nil {
		//nolint:gosec // grpcAddr is normalized to a single-line value before logging.
		slog.Error("failed to listen", "addr", grpcAddr, "err", err)
		return 1
	}

	srv := grpc.NewServer()

	// PingService — Phase 0 connectivity check.
	nfv1.RegisterPingServiceServer(srv, ping.NewHandler())

	// PolicyService — serves dockguard.wasm bundle to NodeKit.
	nfv1.RegisterPolicyServiceServer(srv, policy.NewService())

	// ValidateService — L3 dry-run + L4 smoke run.
	validateSvc, err := validate.NewService()
	if err != nil {
		slog.Warn("ValidateService unavailable (kubeconfig missing?)", "err", err)
	} else {
		nfv1.RegisterValidateServiceServer(srv, validateSvc)
	}

	// ToolRegistryService — CAS storage + index dual-write (gRPC write path).
	registrySvc := catalog.NewToolRegistryService(cat, indexStore)
	nfv1.RegisterToolRegistryServiceServer(srv, registrySvc)

	// DataRegistryService — data artifact registration (gRPC write path).
	dataRegistrySvc := catalog.NewDataRegistryService(dataCat, indexStore)
	nfv1.RegisterDataRegistryServiceServer(srv, dataRegistrySvc)

	// ── Reconcile loops + webhook ─────────────────────────────────────────────
	// Created before BuildService so the reconciler can be injected for eager
	// post-referrer-push health checks (authority map: integrity_health via reconcile axis only).
	fastInterval := parseDuration("NODEVAULT_FAST_RECONCILE", defaultFastReconcile)
	slowInterval := parseDuration("NODEVAULT_SLOW_RECONCILE", defaultSlowReconcile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := startBackground(ctx, indexStore, webhookAddr, fastInterval, slowInterval)

	// BuildService — image build+push → L3 → L4 → registration.
	// rec is passed so BuildService can trigger ReconcileOne after referrer push
	// instead of calling SetIntegrityHealth directly (authority map compliance).
	buildSvc, err := build.NewService(validateSvc, registrySvc, indexStore, rec)
	if err != nil {
		slog.Warn("BuildService unavailable (kubeconfig missing?)", "err", err)
	} else {
		nfv1.RegisterBuildServiceServer(srv, buildSvc)
	}

	//nolint:gosec // listener address is normalized before being attached to logs.
	slog.Info("NodeVault gRPC server starting", "addr", sanitizeLogValue(lis.Addr().String()))

	if serveErr := srv.Serve(lis); serveErr != nil {
		slog.Error("server exited", "err", serveErr)
		return 1
	}
	return 0
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
