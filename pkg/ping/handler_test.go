package ping_test

import (
	"context"
	"strings"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/ping"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func TestPing_PongPrefix(t *testing.T) {
	h := ping.NewHandler()
	resp, err := h.Ping(context.Background(), &nfv1.PingRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(resp.Message, "pong: ") {
		t.Errorf("response %q must start with 'pong: '", resp.Message)
	}
}

func TestPing_ReflectsMessage(t *testing.T) {
	h := ping.NewHandler()
	resp, err := h.Ping(context.Background(), &nfv1.PingRequest{Message: "test-msg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message, "test-msg") {
		t.Errorf("response %q must contain original message", resp.Message)
	}
}

func TestPing_ServerIDPrefix(t *testing.T) {
	h := ping.NewHandler()
	resp, err := h.Ping(context.Background(), &nfv1.PingRequest{Message: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(resp.ServerId, "NodeVault/") {
		t.Errorf("ServerId %q must start with 'NodeVault/'", resp.ServerId)
	}
}

func TestPing_EmptyMessage(t *testing.T) {
	h := ping.NewHandler()
	resp, err := h.Ping(context.Background(), &nfv1.PingRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message != "pong: " {
		t.Errorf("empty message: got %q, want 'pong: '", resp.Message)
	}
}
