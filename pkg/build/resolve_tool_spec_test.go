package build

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func newResolveTestService(t *testing.T) *Service {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return &Service{indexStore: store}
}

func TestResolveToolSpec_FirstRequest_Succeeds(t *testing.T) {
	s := newResolveTestService(t)

	resp, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		Version:  "2.2.1",
		RawSpec:  `{"tool_name":"bwa-mem2"}`,
	})
	if err != nil {
		t.Fatalf("ResolveToolSpec: %v", err)
	}
	if resp.GetToolSpecDigest() == "" {
		t.Fatal("expected non-empty tool_spec_digest")
	}
	if resp.GetToolName() != "bwa-mem2" || resp.GetVersion() != "2.2.1" {
		t.Fatalf("unexpected response fields: %+v", resp)
	}
	if resp.GetResolvedAt() == 0 {
		t.Fatal("expected non-zero resolved_at")
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 resolved tool spec, got %d", len(specs))
	}
	if specs[0].ToolSpecDigest != resp.GetToolSpecDigest() {
		t.Fatalf("digest mismatch: store=%q resp=%q", specs[0].ToolSpecDigest, resp.GetToolSpecDigest())
	}
}

func TestResolveToolSpec_IdenticalRequest_IsIdempotent(t *testing.T) {
	s := newResolveTestService(t)
	req := &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		Version:  "2.2.1",
		RawSpec:  `{"tool_name":"bwa-mem2"}`,
	}

	first, err := s.ResolveToolSpec(context.Background(), req)
	if err != nil {
		t.Fatalf("first ResolveToolSpec: %v", err)
	}

	second, err := s.ResolveToolSpec(context.Background(), req)
	if err != nil {
		t.Fatalf("second ResolveToolSpec: %v", err)
	}

	if first.GetToolSpecDigest() != second.GetToolSpecDigest() {
		t.Fatalf("expected same digest, got %q vs %q", first.GetToolSpecDigest(), second.GetToolSpecDigest())
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected exactly 1 resolved tool spec after duplicate request, got %d", len(specs))
	}
}

func TestResolveToolSpec_EmptyRawSpec_InvalidArgument(t *testing.T) {
	s := newResolveTestService(t)

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  "",
	})
	if err == nil {
		t.Fatal("expected error for empty raw_spec")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_EmptyToolName_InvalidArgument(t *testing.T) {
	s := newResolveTestService(t)

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "",
		RawSpec:  `{"tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error for empty tool_name")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestResolveToolSpec_DifferentRawSpec_DifferentDigest(t *testing.T) {
	s := newResolveTestService(t)

	first, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  `{"tool_name":"bwa-mem2","version":"1"}`,
	})
	if err != nil {
		t.Fatalf("first ResolveToolSpec: %v", err)
	}

	second, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  `{"tool_name":"bwa-mem2","version":"2"}`,
	})
	if err != nil {
		t.Fatalf("second ResolveToolSpec: %v", err)
	}

	if first.GetToolSpecDigest() == second.GetToolSpecDigest() {
		t.Fatal("expected different digests for different raw_spec content")
	}

	specs, err := s.indexStore.ListResolvedToolSpecs()
	if err != nil {
		t.Fatalf("ListResolvedToolSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 resolved tool specs, got %d", len(specs))
	}
}

func TestResolveToolSpec_NilIndexStore_Unavailable(t *testing.T) {
	s := &Service{}

	_, err := s.ResolveToolSpec(context.Background(), &nfv1.ToolSpecRequest{
		ToolName: "bwa-mem2",
		RawSpec:  `{"tool_name":"bwa-mem2"}`,
	})
	if err == nil {
		t.Fatal("expected error when indexStore is nil")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", status.Code(err))
	}
}
