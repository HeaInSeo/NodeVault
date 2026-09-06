// Package grpcauth implements a lightweight shared-secret gate for
// NodeVault's gRPC server.
//
// This is an interim measure, not a replacement for real per-caller
// authentication (mTLS/OIDC/etc — tracked as a separate, larger effort).
// NodeVault's gRPC server currently has no authentication at all; every RPC
// (including SmokeRun's arbitrary Job execution and RegisterTool's direct
// catalog writes) is reachable by any workload that can route to the
// service. This package closes that specific gap for the current threat
// model — other workloads inside the cluster, not the public internet,
// since NodeVault's gRPC port is only exposed via a ClusterIP Service today
// (see deploy/04-grpcroute.yaml, currently unused) — by requiring a shared
// token on every RPC.
//
// Deliberately opt-in: if NODEVAULT_GRPC_SHARED_SECRET is unset, auth is
// disabled and every RPC is served exactly as before. Turning it on is a
// client-breaking change for any existing unauthenticated caller (NodeKit,
// NodeSentinel, ad-hoc grpcurl/test scripts) until they're updated to send
// the token on every call, so it must be a deliberate, coordinated rollout —
// not a silent default.
package grpcauth

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SharedSecretEnv is the NAME of the environment variable holding the
// shared-secret token (not a credential value itself). Unset (the default)
// disables auth entirely.
//
//nolint:gosec // G101: this is an env var NAME, not a hardcoded credential.
const SharedSecretEnv = "NODEVAULT_GRPC_SHARED_SECRET"

// TokenMetadataKey is the NAME of the incoming gRPC metadata key callers
// must set to the configured shared secret (not a credential value
// itself). gRPC metadata keys are lower-cased on the wire.
//
//nolint:gosec // G101: this is a header/metadata key NAME, not a hardcoded credential.
const TokenMetadataKey = "x-nodevault-token"

// FromEnv reads the shared secret from SharedSecretEnv. ok is false when the
// env var is unset or empty, meaning auth is disabled.
func FromEnv() (secret string, ok bool) {
	v := os.Getenv(SharedSecretEnv)
	return v, v != ""
}

// UnaryInterceptor returns a grpc.UnaryServerInterceptor that rejects any
// call whose x-nodevault-token metadata doesn't match secret.
func UnaryInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
	) (any, error) {
		if err := checkToken(ctx, secret); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a grpc.StreamServerInterceptor that rejects any
// call whose x-nodevault-token metadata doesn't match secret. Required
// separately from UnaryInterceptor because gRPC does not run unary
// interceptors for streaming RPCs (e.g. WatchToolBuild).
func StreamInterceptor(secret string) grpc.StreamServerInterceptor {
	return func(
		srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler,
	) error {
		if err := checkToken(ss.Context(), secret); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkToken(ctx context.Context, secret string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing "+TokenMetadataKey+" metadata")
	}
	values := md.Get(TokenMetadataKey)
	if len(values) != 1 || values[0] == "" {
		return status.Error(codes.Unauthenticated, "missing or empty "+TokenMetadataKey+" metadata")
	}
	// Constant-time compare: this is a shared secret, not a public value —
	// a naive == comparison leaks timing information proportional to the
	// length of the matching prefix.
	if subtle.ConstantTimeCompare([]byte(values[0]), []byte(secret)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid "+TokenMetadataKey)
	}
	return nil
}

// HTTPHeaderName is the HTTP header callers must set to the configured
// shared secret for HTTPMiddleware-wrapped endpoints (the Harbor webhook and
// the NodeSentinel validation check/scan-record POST endpoints in
// pkg/catalogrest). Distinct from TokenMetadataKey purely because gRPC
// metadata and HTTP headers are conventionally cased differently; the value
// checked is the same configured secret.
const HTTPHeaderName = "X-NodeVault-Token"

// HTTPMiddleware wraps next, rejecting any request whose HTTPHeaderName
// header doesn't match secret. Used for the catalogrest webhook/validation
// HTTP server — a separate listener from the gRPC server, so it needs its
// own gate even when the gRPC shared secret is enabled.
func HTTPMiddleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(HTTPHeaderName)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			http.Error(w, "missing or invalid "+HTTPHeaderName, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LogStartupState logs whether the shared-secret gate is enabled, loudly —
// matching this repo's existing convention for known, intentional gaps
// (CLAUDE.md D-01/D-02). Call once at startup after FromEnv.
func LogStartupState(enabled bool) {
	if enabled {
		slog.Info("gRPC shared-secret auth ENABLED", "metadata_key", TokenMetadataKey)
		return
	}
	slog.Warn(
		"gRPC shared-secret auth DISABLED — every RPC is reachable by any caller that can " +
			"route to this service (no authentication at all). Set " + SharedSecretEnv +
			" to enable; see pkg/grpcauth doc comment before doing so in a live environment " +
			"(all clients must send the token or every call will start failing).",
	)
}
