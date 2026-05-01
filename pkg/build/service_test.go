package build

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ─── fakeStream — minimal grpc.ServerStreamingServer[nfv1.BuildEvent] mock ───

type fakeStream struct {
	ctx    context.Context
	events []*nfv1.BuildEvent
}

func newFakeStream() *fakeStream { return &fakeStream{ctx: context.Background()} }

func (f *fakeStream) Send(ev *nfv1.BuildEvent) error {
	f.events = append(f.events, ev)
	return nil
}
func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) SetHeader(metadata.MD) error {
	_ = f
	return nil
}
func (f *fakeStream) SendHeader(metadata.MD) error {
	_ = f
	return nil
}
func (f *fakeStream) SetTrailer(metadata.MD) { _ = f }
func (*fakeStream) SendMsg(any) error        { return nil }
func (*fakeStream) RecvMsg(any) error        { return nil }

func (f *fakeStream) kindsSent() []nfv1.BuildEventKind {
	kinds := make([]nfv1.BuildEventKind, 0, len(f.events))
	for _, ev := range f.events {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

// ─── mockBuilder ─────────────────────────────────────────────────────────────

type mockBuilder struct {
	imageID string
	digest  string
	err     error
}

func (m *mockBuilder) Build(
	_ context.Context, _, _ string,
) (imageID, digest string, err error) {
	return m.imageID, m.digest, m.err
}

func (m *mockBuilder) Close() error {
	_ = m
	return nil
}

// ─── registryAddr ─────────────────────────────────────────────────────────────

func TestRegistryAddr_Default(t *testing.T) {
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "")
	if got := registryAddr(); got != defaultRegistryAddr {
		t.Errorf("got %q, want %q", got, defaultRegistryAddr)
	}
}

func TestRegistryAddr_EnvOverride(t *testing.T) {
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "localhost:5000")
	if got := registryAddr(); got != "localhost:5000" {
		t.Errorf("got %q, want %q", got, "localhost:5000")
	}
}

// ─── sanitizeName ─────────────────────────────────────────────────────────────

func TestSanitizeName_LowercasesInput(t *testing.T) {
	if got := sanitizeName("BWA-MEM2"); got != "bwa-mem2" {
		t.Errorf("got %q, want %q", got, "bwa-mem2")
	}
}

func TestSanitizeName_ReplacesSpecialChars(t *testing.T) {
	got := sanitizeName("tool_v1.2@beta")
	if strings.ContainsAny(got, "_@.") {
		t.Errorf("special chars not replaced: %q", got)
	}
}

func TestSanitizeName_TrimsDashes(t *testing.T) {
	got := sanitizeName("---bwa---")
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("leading/trailing dashes not trimmed: %q", got)
	}
}

func TestSanitizeName_TruncatesAt50(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := sanitizeName(long)
	if len(got) > 50 {
		t.Errorf("length %d exceeds 50", len(got))
	}
}

func TestSanitizeName_PreservesAlphanumericAndDash(t *testing.T) {
	in := "bwa-0.7.17"
	got := sanitizeName(in)
	if !strings.Contains(got, "bwa") || !strings.Contains(got, "0") {
		t.Errorf("sanitizeName mangled valid chars: %q → %q", in, got)
	}
}

// ─── BuildAndRegister — mock builder ─────────────────────────────────────────

// TestBuildAndRegister_BuilderError verifies that a build error causes
// BUILD_EVENT_KIND_FAILED to be emitted and an error to be returned.
func TestBuildAndRegister_BuilderError(t *testing.T) {
	svc := &Service{
		builder: &mockBuilder{err: fmt.Errorf("image build backend: exec format error")},
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId: "req-001",
		ToolName:  "test-tool",
		DockerfileContent: `FROM alpine:3.19
RUN echo hello`,
	}

	err := svc.BuildAndRegister(req, stream)
	if err == nil {
		t.Fatal("expected error from BuildAndRegister")
	}
	if !strings.Contains(err.Error(), "image build") {
		t.Errorf("unexpected error: %v", err)
	}

	kinds := stream.kindsSent()
	found := false
	for _, k := range kinds {
		if k == nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED {
			found = true
		}
	}
	if !found {
		t.Errorf("FAILED event not emitted; got %v", kinds)
	}
}

// TestBuildAndRegister_BuilderError_NoSucceededEvent verifies that SUCCEEDED is
// never emitted when the image build fails.
func TestBuildAndRegister_BuilderError_NoSucceededEvent(t *testing.T) {
	svc := &Service{
		builder: &mockBuilder{err: fmt.Errorf("layer not found")},
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-002", ToolName: "bwa"}

	_ = svc.BuildAndRegister(req, stream)

	for _, k := range stream.kindsSent() {
		if k == nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED {
			t.Error("SUCCEEDED event must not be emitted when build fails")
		}
	}
}

// TestBuildAndRegister_BuilderError_JobCreatedFirst verifies that JOB_CREATED is
// emitted before FAILED (build was attempted, then failed).
func TestBuildAndRegister_BuilderError_JobCreatedFirst(t *testing.T) {
	svc := &Service{
		builder: &mockBuilder{err: fmt.Errorf("context deadline exceeded")},
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-003", ToolName: "samtools"}

	_ = svc.BuildAndRegister(req, stream)

	kinds := stream.kindsSent()
	if len(kinds) < 2 {
		t.Fatalf("expected at least 2 events, got %v", kinds)
	}
	if kinds[0] != nfv1.BuildEventKind_BUILD_EVENT_KIND_JOB_CREATED {
		t.Errorf("first event: got %v, want JOB_CREATED", kinds[0])
	}
}
