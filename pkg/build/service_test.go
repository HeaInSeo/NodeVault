package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/registryconfig"
	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const validPolicyDockerfile = "FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"

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
	imageID       string
	digest        string
	layerCacheHit bool
	err           error

	pushTagErr   error
	pushTagCalls []string // destinations passed to PushTag, in call order
}

func (m *mockBuilder) Build(
	_ context.Context, _, _ string,
) (imageID, digest string, layerCacheHit bool, err error) {
	return m.imageID, m.digest, m.layerCacheHit, m.err
}

func (m *mockBuilder) PushTag(_ context.Context, _, destination string) (digest string, err error) {
	m.pushTagCalls = append(m.pushTagCalls, destination)
	if m.pushTagErr != nil {
		return "", m.pushTagErr
	}
	return m.digest, nil
}

func (m *mockBuilder) Close() error {
	_ = m
	return nil
}

// ─── registryAddr ─────────────────────────────────────────────────────────────

func TestRegistryAddr_Default(t *testing.T) {
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "")
	if got := registryAddr(); got != registryconfig.DefaultAddr {
		t.Errorf("got %q, want %q", got, registryconfig.DefaultAddr)
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
		RequestId:         "req-001",
		ToolName:          "test-tool",
		DockerfileContent: validPolicyDockerfile,
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

func TestBuildAndRegister_InvalidDockerfileRejectedBeforeBuilder(t *testing.T) {
	buildCalls := 0
	svc := &Service{
		builder: &countCallBuilder{inner: &mockBuilder{}, calls: &buildCalls},
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId:         "req-policy-001",
		ToolName:          "test-tool",
		DockerfileContent: "FROM alpine:latest\nRUN true",
	}

	err := svc.BuildAndRegister(req, stream)
	if err == nil || !strings.Contains(err.Error(), "build request policy") {
		t.Fatalf("BuildAndRegister error got %v, want policy rejection", err)
	}
	if buildCalls != 0 {
		t.Fatalf("Build called %d times, want 0", buildCalls)
	}

	kinds := stream.kindsSent()
	if len(kinds) != 1 || kinds[0] != nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED {
		t.Fatalf("events got %v, want exactly FAILED", kinds)
	}
}

// TestBuildAndRegister_RootlessFailure_NoPrivilegedFallback verifies that a
// rootless Buildah error (e.g. mknod EPERM in user namespace) emits
// BUILD_EVENT_KIND_FAILED and does not fall back to a privileged build path.
// NodeVault has no retry or escalation logic — builder.Build() is called
// exactly once and any error is terminal.
func TestBuildAndRegister_RootlessFailure_NoPrivilegedFallback(t *testing.T) {
	rootlessErr := fmt.Errorf(
		"build image: error building at STEP 2: error processing " +
			"RUN mknod /dev/test c 1 3: exit status 1: mknod: /dev/test: Operation not permitted",
	)

	buildCalls := 0
	svc := &Service{
		builder: &countCallBuilder{inner: &mockBuilder{err: rootlessErr}, calls: &buildCalls},
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId:         "req-rootless-01",
		ToolName:          "test-tool",
		DockerfileContent: "FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN mknod /dev/test c 1 3",
	}

	if err := svc.BuildAndRegister(req, stream); err == nil {
		t.Fatal("expected error from rootless build failure, got nil")
	}

	kinds := stream.kindsSent()
	var failedCount, succeededCount int
	for _, k := range kinds {
		switch k {
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED:
			failedCount++
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED:
			succeededCount++
		}
	}
	if failedCount == 0 {
		t.Errorf("FAILED event not emitted; got %v", kinds)
	}
	if succeededCount > 0 {
		t.Errorf("SUCCEEDED must never be emitted after rootless failure (no privileged fallback)")
	}
	if buildCalls != 1 {
		t.Errorf("Build called %d times, want exactly 1 — no privileged retry", buildCalls)
	}
}

// countCallBuilder wraps a Builder and counts Build invocations.
type countCallBuilder struct {
	inner Builder
	calls *int
}

func (c *countCallBuilder) Build(
	ctx context.Context, dockerfile, outputRef string,
) (imageID, digest string, layerCacheHit bool, err error) {
	*c.calls++
	return c.inner.Build(ctx, dockerfile, outputRef)
}

func (c *countCallBuilder) PushTag(
	ctx context.Context, localRef, destination string,
) (digest string, err error) {
	return c.inner.PushTag(ctx, localRef, destination)
}

func (c *countCallBuilder) Close() error { return c.inner.Close() }

// TestBuildAndRegister_BuilderError_NoSucceededEvent verifies that SUCCEEDED is
// never emitted when the image build fails.
func TestBuildAndRegister_BuilderError_NoSucceededEvent(t *testing.T) {
	svc := &Service{
		builder: &mockBuilder{err: fmt.Errorf("layer not found")},
	}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-002", ToolName: "bwa", DockerfileContent: validPolicyDockerfile}

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
	req := &nfv1.BuildRequest{RequestId: "req-003", ToolName: "samtools", DockerfileContent: validPolicyDockerfile}

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
	req := &nfv1.BuildRequest{RequestId: "req-fail-001", ToolName: "test-tool", DockerfileContent: validPolicyDockerfile}

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
	svc.recordBuildSuccess("build-xyz", startedAt, "sha256:imgdigest", "harbor.example.com/library/tool:latest", true)

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
	if rec.Execution == nil || rec.Execution.Mode != backendInPodBuildah || rec.Execution.HostUsers == nil || *rec.Execution.HostUsers {
		t.Fatalf("Execution: got %+v, want in-pod-buildah with host_users=false", rec.Execution)
	}
	if rec.Execution.LayerCacheHit == nil || !*rec.Execution.LayerCacheHit {
		t.Errorf("Execution.LayerCacheHit: got %v, want true", rec.Execution.LayerCacheHit)
	}
	if rec.Backend != backendInPodBuildah {
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
	svc.recordBuildSuccess("build-noop", mustParseTime(t), "sha256:x", "ref", false)
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

// ─── postBuildRegistration (AC-REG-04) ────────────────────────────────────────

type fakeReconciler struct {
	calledWith []string
}

func (f *fakeReconciler) ReconcileOne(_ context.Context, casHash string) error {
	f.calledWith = append(f.calledWith, casHash)
	return nil
}

// TestPostBuildRegistration_ReferrerPushFailure_TriggersReconcile verifies
// AC-REG-04: even when spec referrer push fails (here, deterministically, by
// pointing the registry at a closed local port so PushToolSpecReferrer fails
// fast), ReconcileOne must still be called so integrity_health converges
// toward Partial without waiting for the next reconcile tick — and
// lifecycle_phase must stay Active regardless of referrer push outcome.
func TestPostBuildRegistration_ReferrerPushFailure_TriggersReconcile(t *testing.T) {
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "127.0.0.1:1") // connection refused, fails fast

	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	cat := catalog.NewCatalogAt(t.TempDir())
	registrySvc := catalog.NewToolRegistryService(cat, store)
	rec := &fakeReconciler{}
	svc := &Service{registry: registrySvc, indexStore: store, reconciler: rec}

	var logs []string
	req := &nfv1.BuildRequest{RequestId: "req-1", ToolName: "test-tool", Version: "1.0.0"}
	if err := svc.postBuildRegistration(context.Background(), req, "harbor.example.com/library/test-tool@sha256:deadbeef", "sha256:deadbeef",
		func(msg string) { logs = append(logs, msg) }); err != nil {
		t.Fatalf("postBuildRegistration: %v (registration itself must succeed; only the referrer push fails)", err)
	}

	if len(rec.calledWith) != 1 {
		t.Fatalf("expected ReconcileOne called once despite referrer push failure, got %d calls: %v", len(rec.calledWith), rec.calledWith)
	}

	foundFailureLog := false
	for _, l := range logs {
		if strings.Contains(l, "spec referrer push failed") {
			foundFailureLog = true
		}
	}
	if !foundFailureLog {
		t.Errorf("expected a %q log line, got %v", "spec referrer push failed", logs)
	}

	entries, lerr := store.All()
	if lerr != nil {
		t.Fatalf("store.All: %v", lerr)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 index entry, got %d", len(entries))
	}
	if entries[0].LifecyclePhase != index.PhaseActive {
		t.Errorf("LifecyclePhase: got %q, want Active (referrer push failure must not affect lifecycle_phase)", entries[0].LifecyclePhase)
	}
}

// fakeSentinel counts EnqueueValidationWork calls, for asserting registration
// failure skips sentinel enqueue entirely (#23).
type fakeSentinel struct {
	calls int
}

func (f *fakeSentinel) EnqueueValidationWork(
	_ context.Context, _ *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	f.calls++
	return &nsv1.EnqueueValidationWorkResponse{JobId: "job-1", Status: "Queued"}, nil
}

// TestPostBuildRegistration_RegisterToolFailure_ReturnsErrorAndSkipsSentinelEnqueue
// is #23's core regression guard: a genuine RegisterTool failure (here,
// deterministically, by deleting the catalog's backing directory so its
// CAS write fails) must surface as an error from postBuildRegistration, and
// must not enqueue NodeSentinel validation work for a tool that was never
// actually registered.
func TestPostBuildRegistration_RegisterToolFailure_ReturnsErrorAndSkipsSentinelEnqueue(t *testing.T) {
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	catDir := t.TempDir()
	cat := catalog.NewCatalogAt(catDir)
	if err := os.RemoveAll(catDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	registrySvc := catalog.NewToolRegistryService(cat, store)
	sentinel := &fakeSentinel{}
	svc := &Service{registry: registrySvc, indexStore: store, sentinel: sentinel}

	req := &nfv1.BuildRequest{RequestId: "req-1", ToolName: "test-tool", Version: "1.0.0"}
	regErr := svc.postBuildRegistration(context.Background(), req,
		"harbor.example.com/library/test-tool:1.0.0", "sha256:deadbeef", func(string) {})
	if regErr == nil {
		t.Fatal("expected an error when the catalog directory does not exist")
	}
	if sentinel.calls != 0 {
		t.Errorf("sentinel enqueue calls = %d, want 0 (must not enqueue for an unregistered tool)", sentinel.calls)
	}
}

// TestBuildAndRegister_RegistrationFailure_EmitsFAILEDNotSUCCEEDED verifies
// the legacy streaming path: a registration failure must emit FAILED (not
// SUCCEEDED) and return an error, with the digest already acquired earlier
// in the stream preserved in the FAILED event's message.
func TestBuildAndRegister_RegistrationFailure_EmitsFAILEDNotSUCCEEDED(t *testing.T) {
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	catDir := t.TempDir()
	cat := catalog.NewCatalogAt(catDir)
	if err := os.RemoveAll(catDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	registrySvc := catalog.NewToolRegistryService(cat, store)
	svc := &Service{builder: &mockBuilder{digest: "sha256:regfail"}, registry: registrySvc, indexStore: store}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId:         "req-regfail-1",
		ToolName:          "bwa",
		DockerfileContent: validPolicyDockerfile,
	}

	if err := svc.BuildAndRegister(req, stream); err == nil {
		t.Fatal("BuildAndRegister: expected an error when registration fails")
	}

	var failedMsg string
	sawSucceeded := false
	for _, ev := range stream.events {
		switch ev.Kind {
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED:
			sawSucceeded = true
		case nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED:
			failedMsg = ev.Message
		}
	}
	if sawSucceeded {
		t.Error("SUCCEEDED must not be emitted when registration fails")
	}
	if !strings.Contains(failedMsg, "sha256:regfail") {
		t.Errorf("FAILED message = %q, want it to preserve the already-pushed digest", failedMsg)
	}
}

// TestSubmitToolBuild_RegistrationFailure_TransitionsToFailedWithDigestPreserved
// verifies the SubmitToolBuild/WatchToolBuild path: a registration failure
// must transition buildstate to Failed (not Succeeded), and the terminal
// event must still carry the image ref/digest that SetArtifact already
// persisted before registration was attempted.
func TestSubmitToolBuild_RegistrationFailure_TransitionsToFailedWithDigestPreserved(t *testing.T) {
	svc := newSubmitTestService(t)
	catDir := t.TempDir()
	cat := catalog.NewCatalogAt(catDir)
	if err := os.RemoveAll(catDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	svc.registry = catalog.NewToolRegistryService(cat, svc.indexStore)

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-regfail", ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-regfail"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	last := stream.events[len(stream.events)-1]
	if last.GetStatus() != string(buildstate.StatusFailed) {
		t.Fatalf("final status = %q, want Failed", last.GetStatus())
	}
	if last.GetImageDigest() == "" {
		t.Error("ImageDigest should remain populated on the FAILED terminal event")
	}
}

// ─── disabled backend ─────────────────────────────────────────────────────────

// TestNewDisabledService_ReturnsDisabledBackendError verifies that NewDisabledService
// returns a service that immediately responds with ErrBuildBackendDisabled.
// The error text must contain "disabled" so operators can identify spike mode.
func TestNewDisabledService_ReturnsDisabledBackendError(t *testing.T) {
	svc := NewDisabledService()
	stream := newFakeStream()
	req := &nfv1.BuildRequest{RequestId: "req-disabled-01", ToolName: "bwa", DockerfileContent: validPolicyDockerfile}

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
	req := &nfv1.BuildRequest{RequestId: "req-disabled-02", ToolName: "samtools", DockerfileContent: validPolicyDockerfile}

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
	req := &nfv1.BuildRequest{RequestId: "req-disabled-03", ToolName: "gatk", DockerfileContent: validPolicyDockerfile}

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

// ─── versioned tagging (#27) ──────────────────────────────────────────────────

func TestSanitizeTag_PreservesAlreadySafeVersions(t *testing.T) {
	for _, v := range []string{"0.7.17", "1.2.3-r1", "2026.07"} {
		if got := sanitizeTag(v); got != v {
			t.Errorf("sanitizeTag(%q) = %q, want unchanged", v, got)
		}
	}
}

// TestSanitizeTag_DifferentInputsNeverCollide guards the exact scenario
// flagged in review: naive character substitution alone would map both
// "1.0+cuda" and "1.0/cuda" to "1.0-cuda", silently conflating two
// different versions under one tag.
func TestSanitizeTag_DifferentInputsNeverCollide(t *testing.T) {
	inputs := []string{"1.0+cuda", "1.0/cuda", "v1.0+cuda/12", "1.0 alpha", "../../../latest"}
	seen := make(map[string]string, len(inputs))
	for _, in := range inputs {
		got := sanitizeTag(in)
		if got == "" {
			t.Errorf("sanitizeTag(%q) = \"\", want a non-empty tag", in)
			continue
		}
		if prior, ok := seen[got]; ok {
			t.Errorf("sanitizeTag collision: %q and %q both produced %q", prior, in, got)
		}
		seen[got] = in
	}
}

func TestSanitizeTag_EmptyInput(t *testing.T) {
	if got := sanitizeTag(""); got != "" {
		t.Errorf("sanitizeTag(\"\") = %q, want \"\"", got)
	}
}

func TestPrimaryBuildDestination_UsesVersionWhenAvailable(t *testing.T) {
	dest, isVersioned := primaryBuildDestination("bwa", "0.7.17")
	if !isVersioned {
		t.Fatal("isVersioned = false, want true")
	}
	if !strings.HasSuffix(dest, ":0.7.17") {
		t.Errorf("destination = %q, want suffix :0.7.17", dest)
	}
}

func TestPrimaryBuildDestination_FallsBackToLatestWhenVersionEmpty(t *testing.T) {
	dest, isVersioned := primaryBuildDestination("bwa", "")
	if isVersioned {
		t.Fatal("isVersioned = true, want false")
	}
	if !strings.HasSuffix(dest, ":latest") {
		t.Errorf("destination = %q, want suffix :latest", dest)
	}
}

// TestBuildAndRegister_VersionedBuild_PushesLatestAlias verifies the primary
// build/push targets the version-pinned tag, and :latest is pushed
// separately as a best-effort alias — the inverse of the original (broken)
// design where every build only ever pushed :latest.
func TestBuildAndRegister_VersionedBuild_PushesLatestAlias(t *testing.T) {
	builder := &mockBuilder{digest: "sha256:versioned"}
	svc := &Service{builder: builder}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId:         "req-ver-1",
		ToolName:          "bwa",
		Version:           "0.7.17",
		DockerfileContent: validPolicyDockerfile,
	}

	if err := svc.BuildAndRegister(req, stream); err != nil {
		t.Fatalf("BuildAndRegister: %v", err)
	}

	var pushedDigest string
	for _, ev := range stream.events {
		if ev.Kind == nfv1.BuildEventKind_BUILD_EVENT_KIND_JOB_CREATED {
			if !strings.Contains(ev.Message, ":0.7.17") {
				t.Errorf("JOB_CREATED message = %q, want the version-pinned destination", ev.Message)
			}
		}
		if ev.Kind == nfv1.BuildEventKind_BUILD_EVENT_KIND_DIGEST_ACQUIRED {
			pushedDigest = ev.Digest
		}
	}
	if pushedDigest != "sha256:versioned" {
		t.Errorf("DIGEST_ACQUIRED digest = %q", pushedDigest)
	}
	if len(builder.pushTagCalls) != 1 || !strings.HasSuffix(builder.pushTagCalls[0], ":latest") {
		t.Errorf("PushTag calls = %v, want exactly one call to a :latest destination", builder.pushTagCalls)
	}
}

// TestBuildAndRegister_NoVersion_NoLatestAliasPush verifies that when no
// version is available, Build's primary destination is already :latest, so
// no separate PushTag call happens (nothing to alias).
func TestBuildAndRegister_NoVersion_NoLatestAliasPush(t *testing.T) {
	builder := &mockBuilder{digest: "sha256:novers"}
	svc := &Service{builder: builder}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId:         "req-novers-1",
		ToolName:          "bwa",
		DockerfileContent: validPolicyDockerfile,
	}

	if err := svc.BuildAndRegister(req, stream); err != nil {
		t.Fatalf("BuildAndRegister: %v", err)
	}
	if len(builder.pushTagCalls) != 0 {
		t.Errorf("PushTag calls = %v, want none", builder.pushTagCalls)
	}
}

// TestBuildAndRegister_LatestAliasPushFails_BuildStillSucceeds is the
// partial-failure case from review: a version tag push failure must fail
// the build (it's Build's primary destination, already covered by
// TestBuildAndRegister_BuilderError), but a :latest alias push failure must
// NOT — the build itself already succeeded and latest is a convenience
// pointer, not the authoritative reference.
func TestBuildAndRegister_LatestAliasPushFails_BuildStillSucceeds(t *testing.T) {
	builder := &mockBuilder{digest: "sha256:aliasfail", pushTagErr: errors.New("registry unavailable")}
	svc := &Service{builder: builder}
	stream := newFakeStream()
	req := &nfv1.BuildRequest{
		RequestId:         "req-aliasfail-1",
		ToolName:          "bwa",
		Version:           "0.7.17",
		DockerfileContent: validPolicyDockerfile,
	}

	if err := svc.BuildAndRegister(req, stream); err != nil {
		t.Fatalf("BuildAndRegister: %v, want nil — latest alias failure must be non-fatal", err)
	}
	found := false
	for _, k := range stream.kindsSent() {
		if k == nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED {
			found = true
		}
	}
	if !found {
		t.Error("expected SUCCEEDED event despite latest alias push failure")
	}
}

// TestWarnIfTagReassigned_DoesNotErrorOrPanic exercises the tag-reassignment
// detection path (rebuild of the same version producing a different
// digest): warnIfTagReassigned must not error or block recording, and the
// prior build's record must remain queryable afterward (ToolImageRecord's
// (ImageDigest, BuildID) composite key already guarantees this — see the
// bare-conda-package-bypass fix commit).
func TestWarnIfTagReassigned_DoesNotErrorOrPanic(t *testing.T) {
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	svc := &Service{builder: &mockBuilder{}, indexStore: store}
	const dest = "harbor.example.com/library/bwa:0.7.17"

	svc.recordBuildSuccess("build-1", mustParseTime(t), "sha256:first", dest, false)
	svc.warnIfTagReassigned(dest, "sha256:second") // must not panic/error
	svc.recordBuildSuccess("build-2", mustParseTime(t), "sha256:second", dest, false)

	first, err := store.GetToolBuildRecordByBuildID("build-1")
	if err != nil || first.ImageDigest != "sha256:first" {
		t.Errorf("build-1 record lost or wrong after reassignment: %+v, err=%v", first, err)
	}
	latest, err := store.GetLatestToolImageRecordByRef(dest)
	if err != nil || latest.ImageDigest != "sha256:second" {
		t.Errorf("GetLatestToolImageRecordByRef(%q) = %+v, err=%v, want sha256:second", dest, latest, err)
	}
}
