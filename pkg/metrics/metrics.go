// Package metrics exposes NodeVault operational counters via expvar.
// The HTTP server at /debug/vars (expvar JSON) and /healthz is started by StartServer.
// All counters are monotonically increasing — use delta queries to measure rates.
package metrics

import (
	"expvar"
	"log/slog"
	"net/http"
	"time"
)

// Counters — monotonically increasing since process start.
var (
	ReconcileFastTotal  = expvar.NewInt("nodevault_reconcile_fast_total")
	ReconcileSlowTotal  = expvar.NewInt("nodevault_reconcile_slow_total")
	ReconcileErrorTotal = expvar.NewInt("nodevault_reconcile_error_total")
	BuildSuccessTotal   = expvar.NewInt("nodevault_build_success_total")
	BuildFailureTotal   = expvar.NewInt("nodevault_build_failure_total")
)

// StartServer starts the metrics HTTP server (non-blocking).
// Endpoints: /debug/vars (expvar JSON), /healthz.
// addr defaults to ":9090" if empty.
func StartServer(addr string) {
	if addr == "" {
		addr = ":9090"
	}
	mux := http.NewServeMux()
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	go func() {
		srv := &http.Server{
			Addr:         addr,
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		slog.Info("metrics server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server exited", "err", err)
		}
	}()
}
