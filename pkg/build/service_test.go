package build

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/HeaInSeo/NodeVault/pkg/index"
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

// ─── ToolBuildRecord / ToolImageRecord index recording ───────────────────────

// TestBuildAndRegister_BuilderError_RecordsFailedToolBuildRecord verifies that a
// build failure is recorded as a ToolBuildRecord with Success=false and a
// FailureReason, completing Sprint 1's "BuildAndRegister 후 Index에 조회됨" goal
// for the failure path.
func TestBuildAndRegister_BuilderError_RecordsFailedToolBuildRecord(t *testing.T) {
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := &Service{
		builder:    &mockBuilder{err: fmt.Errorf("image build backend: exec format error")},
		indexStore: store,
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-fail-001", ToolName: "test-tool"}

	if err := svc.BuildAndRegister(req, stream); err == nil {
		t.Fatal("expected error from BuildAndRegister")
	}

	builds, lerr := store.ListToolBuildRecordsByToolSpecDigest("")
	if lerr != nil {
		t.Fatalf("ListToolBuildRecordsByToolSpecDigest: %v", lerr)
	}
	// ToolSpecDigest is not wired yet (Sprint 1 scope), so failed records carry "".
	if len(builds) != 1 {
		t.Fatalf("expected 1 ToolBuildRecord, got %d", len(builds))
	}
	if builds[0].Success {
		t.Error("Success: got true, want false")
	}
	if builds[0].FailureReason == "" {
		t.Error("FailureReason should be populated")
	}
	if !strings.HasPrefix(builds[0].BuildID, "req-fail-001-") {
		t.Errorf("BuildID: got %q, want prefix %q", builds[0].BuildID, "req-fail-001-")
	}
}

// TestRecordBuildSuccess_WritesToolBuildRecordAndToolImageRecord verifies the
// pkg/build → pkg/index integration point in isolation from the L3/L4 validator,
// which requires a live Kubernetes client and is out of scope for this unit test.
func TestRecordBuildSuccess_WritesToolBuildRecordAndToolImageRecord(t *testing.T) {
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := &Service{builder: &mockBuilder{}, indexStore: store}

	startedAt := mustParseTime(t)
	svc.recordBuildSuccess("build-xyz", startedAt, "sha256:imgdigest", "harbor.example.com/library/tool:latest")

	rec, gerr := store.GetToolBuildRecordByBuildID("build-xyz")
	if gerr != nil {
		t.Fatalf("GetToolBuildRecordByBuildID: %v", gerr)
	}
	if !rec.Success {
		t.Error("Success: got false, want true")
	}
	if rec.ImageDigest != "sha256:imgdigest" {
		t.Errorf("ImageDigest: got %q", rec.ImageDigest)
	}
	if rec.Execution == nil || rec.Execution.Mode != "in-pod-buildah" || rec.Execution.HostUsers == nil || *rec.Execution.HostUsers {
		t.Fatalf("Execution: got %+v, want in-pod-buildah with host_users=false", rec.Execution)
	}
	if rec.Backend != "in-pod-buildah" {
		t.Errorf("Backend: got %q, want in-pod-buildah", rec.Backend)
	}

	img, ierr := store.GetToolImageRecordByDigest("sha256:imgdigest")
	if ierr != nil {
		t.Fatalf("GetToolImageRecordByDigest: %v", ierr)
	}
	if img.BuildID != "build-xyz" {
		t.Errorf("BuildID: got %q, want build-xyz", img.BuildID)
	}
	if img.ImageRef != "harbor.example.com/library/tool:latest" {
		t.Errorf("ImageRef: got %q", img.ImageRef)
	}
}

// TestRecordBuildFailure_WritesFailedToolBuildRecord verifies recordBuildFailure
// in isolation.
func TestRecordBuildFailure_WritesFailedToolBuildRecord(t *testing.T) {
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := &Service{builder: &mockBuilder{}, indexStore: store}

	startedAt := mustParseTime(t)
	svc.recordBuildFailure("build-failed-1", startedAt, errors.New("kaniko exited 1"))

	rec, gerr := store.GetToolBuildRecordByBuildID("build-failed-1")
	if gerr != nil {
		t.Fatalf("GetToolBuildRecordByBuildID: %v", gerr)
	}
	if rec.Success {
		t.Error("Success: got true, want false")
	}
	if rec.FailureReason != "kaniko exited 1" {
		t.Errorf("FailureReason: got %q", rec.FailureReason)
	}
}

// TestRecordBuildSuccess_NilIndexStore_NoOp verifies that recording is a safe
// no-op when no Store is wired (e.g. NewDisabledService).
func TestRecordBuildSuccess_NilIndexStore_NoOp(t *testing.T) {
	svc := &Service{builder: &mockBuilder{}}
	// Must not panic.
	svc.recordBuildSuccess("build-noop", mustParseTime(t), "sha256:x", "ref")
	svc.recordBuildFailure("build-noop-2", mustParseTime(t), errors.New("err"))
}

func mustParseTime(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, "2026-06-15T00:00:00Z")
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}
	return parsed
}

// ─── disabled backend ─────────────────────────────────────────────────────────

// TestNewDisabledService_ReturnsDisabledBackendError verifies that NewDisabledService
// returns a service that immediately responds with ErrBuildBackendDisabled.
// The error text must contain "disabled" so operators can identify spike mode.
func TestNewDisabledService_ReturnsDisabledBackendError(t *testing.T) {
	svc := NewDisabledService()
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-disabled-01", ToolName: "bwa"}

	err := svc.BuildAndRegister(req, stream)
	if err == nil {
		t.Fatal("expected error from disabled backend, got nil")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error should mention 'disabled', got: %v", err)
	}
}

// TestNewDisabledService_EmitsFAILEDEvent verifies that the gRPC stream receives
// a FAILED event (server stays alive; only the RPC fails).
func TestNewDisabledService_EmitsFAILEDEvent(t *testing.T) {
	svc := NewDisabledService()
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-disabled-02", ToolName: "samtools"}

	_ = svc.BuildAndRegister(req, stream)

	found := false
	for _, k := range stream.kindsSent() {
		if k == nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED {
			found = true
		}
	}
	if !found {
		t.Errorf("FAILED event not emitted by disabled backend; got %v", stream.kindsSent())
	}
}

// TestNewDisabledService_NoSUCCEEDEDEvent verifies SUCCEEDED is never emitted.
func TestNewDisabledService_NoSUCCEEDEDEvent(t *testing.T) {
	svc := NewDisabledService()
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-disabled-03", ToolName: "gatk"}

	_ = svc.BuildAndRegister(req, stream)

	for _, k := range stream.kindsSent() {
		if k == nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED {
			t.Error("SUCCEEDED must never be emitted by disabled backend")
		}
	}
}

// TestDisabledBuilder_Close verifies Close is a no-op.
func TestDisabledBuilder_Close(t *testing.T) {
	b := disabledBuilder{}
	if err := b.Close(); err != nil {
		t.Errorf("Close should return nil, got %v", err)
	}
}
