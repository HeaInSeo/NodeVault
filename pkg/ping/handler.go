// Package ping implements the PingService gRPC handler.
// Used in Phase 0 to verify NodeKit ↔ NodeVault gRPC connectivity.
package ping

import (
	"context"
	"os"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// Handler implements nfv1.PingServiceServer.
type Handler struct {
	nfv1.UnimplementedPingServiceServer
}

// NewHandler creates a PingService handler.
func NewHandler() *Handler {
	return &Handler{}
}

// Ping responds with pong and the server hostname.
func (h *Handler) Ping(_ context.Context, req *nfv1.PingRequest) (*nfv1.PingResponse, error) {
	_ = h
	host, _ := os.Hostname()
	return &nfv1.PingResponse{
		Message:  "pong: " + req.GetMessage(),
		ServerId: "NodeVault/" + host,
	}, nil
}
