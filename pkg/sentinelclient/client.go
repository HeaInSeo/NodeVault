// Package sentinelclient is a thin gRPC client for NodeSentinel's IngressService.
// NodeVault calls EnqueueValidationWork after a successful build+push so NodeSentinel
// can run L3/L4 validation work asynchronously. NodeVault does not implement any
// NodeSentinel server logic — this package is a client only.
package sentinelclient

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"
)

const defaultGRPCAddr = "nodesentinel.nodesentinel-system.svc.cluster.local:50052"

const enqueueTimeout = 5 * time.Second

// grpcAddr returns the NodeSentinel ingress address, overridable via
// NODESENTINEL_GRPC_ADDR (same convention as NODEVAULT_REGISTRY_ADDR).
func grpcAddr() string {
	if v := os.Getenv("NODESENTINEL_GRPC_ADDR"); v != "" {
		return v
	}
	return defaultGRPCAddr
}

// Addr returns the NodeSentinel ingress address that New() would dial —
// the NODESENTINEL_GRPC_ADDR override, or the in-cluster default. Exposed
// for startup logging so operators can see the resolved address without
// duplicating the override lookup.
func Addr() string {
	return grpcAddr()
}

// Client is a gRPC client for NodeSentinel's IngressService.
type Client struct {
	conn   *grpc.ClientConn
	ingest nsv1.IngressServiceClient
}

// New dials NodeSentinel's ingress address (NODESENTINEL_GRPC_ADDR, or the
// in-cluster default) and returns a ready-to-use Client.
func New() (*Client, error) {
	conn, err := grpc.NewClient(grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("sentinelclient: dial %s: %w", grpcAddr(), err)
	}
	return &Client{conn: conn, ingest: nsv1.NewIngressServiceClient(conn)}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// EnqueueValidationWork asks NodeSentinel to queue L3/L4 validation work for a
// freshly built+pushed artifact. The call is bounded by a 5s timeout regardless
// of the caller's context deadline.
func (c *Client) EnqueueValidationWork(
	ctx context.Context, req *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()

	resp, err := c.ingest.EnqueueValidationWork(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sentinelclient: EnqueueValidationWork: %w", err)
	}
	return resp, nil
}
