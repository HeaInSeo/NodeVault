package build

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func newSubmitTestService(t *testing.T) *Service {
	t.Helper()
	idx, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	state, err := buildstate.Open(filepath.Join(t.TempDir(), "build-state.db"))
	if err != nil {
		t.Fatalf("buildstate.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Fatalf("buildstate.Close: %v", err)
		}
	})
	if err := idx.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-123",
		ToolName:       "bwa-mem2",
		Version:        "2.2.1",
		RawSpec:        `{"tool_name":"bwa-mem2","version":"2.2.1","dockerfile_content":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"}`,
		ResolvedAt:     time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}
	return &Service{
		builder:    &mockBuilder{digest: "sha256:built"},
		indexStore: idx,
		buildState: state,
	}
}

// TestBuildRequestFromResolved_DeserializesAllowRuntimeTools verifies that
// allow_runtime_tools/allow_runtime_tools_reason round-trip through the
// raw_spec JSON the same way every other BuildRequest field does — these are
// plain proto-generated JSON tags, not a bespoke unmarshal path.
func TestBuildRequestFromResolved_DeserializesAllowRuntimeTools(t *testing.T) {
	spec := index.ResolvedToolSpec{
		ToolSpecDigest: "spec-allow-1",
		ToolName:       "bwa",
		RawSpec: `{"tool_name":"bwa","dockerfile_content":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN curl -fsSL -o out https://example.com",` +
			`"allow_runtime_tools":["curl"],"allow_runtime_tools_reason":"plugin catalog fetch"}`,
	}
	req, err := buildRequestFromResolved("build-allow-1", spec)
	if err != nil {
		t.Fatalf("buildRequestFromResolved: %v", err)
	}
	if len(req.GetAllowRuntimeTools()) != 1 || req.GetAllowRuntimeTools()[0] != "curl" {
		t.Fatalf("AllowRuntimeTools: got %v, want [curl]", req.GetAllowRuntimeTools())
	}
	if req.GetAllowRuntimeToolsReason() != "plugin catalog fetch" {
		t.Fatalf("AllowRuntimeToolsReason: got %q", req.GetAllowRuntimeToolsReason())
	}
	if err := ValidateBuildRequest(req); err != nil {
		t.Fatalf("ValidateBuildRequest should accept curl with the deserialized exemption: %v", err)
	}
}

// TestBuildRequestFromResolved_UnknownField_Rejected guards against a raw_spec
// typo or contract-drift field silently being ignored: NodeKit's only
// producer (ToolSpecRawSpecFactory.Build) emits only real BuildRequest
// fields, so this should never fire in practice — but a typo (e.g.
// "dockerfile_kontent") must surface a clear decode error instead of
// silently producing a BuildRequest with an empty DockerfileContent.
func TestBuildRequestFromResolved_UnknownField_Rejected(t *testing.T) {
	spec := index.ResolvedToolSpec{
		ToolSpecDigest: "spec-unknown-1",
		ToolName:       "bwa",
		RawSpec:        `{"tool_name":"bwa","dockerfile_kontent":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"}`,
	}
	_, err := buildRequestFromResolved("build-unknown-1", spec)
	if err == nil {
		t.Fatal("buildRequestFromResolved: expected an error for an unrecognized raw_spec field, got nil")
	}
}

func TestSubmitToolBuild_CreatesRequestedBuildState(t *testing.T) {
	svc := newSubmitTestService(t)

	resp, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-123",
		ToolSpecDigest: "spec-123",
	})
	if err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	if resp.GetBuildId() != "build-123" || resp.GetToolSpecDigest() != "spec-123" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.GetStatus() != string(buildstate.StatusRequested) {
		t.Fatalf("status got %q want Requested", resp.GetStatus())
	}

	got, err := svc.buildState.Get("build-123")
	if err != nil {
		t.Fatalf("buildState.Get: %v", err)
	}
	if got.Status != buildstate.StatusRequested && got.Status != buildstate.StatusResolving && got.Status != buildstate.StatusBuilding && !buildstate.Terminal(got.Status) {
		t.Fatalf("stored status got %q, want a valid submitted lifecycle state", got.Status)
	}
}

func TestSubmitToolBuild_UnknownToolSpec_NotFound(t *testing.T) {
	svc := newSubmitTestService(t)

	_, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-unknown",
		ToolSpecDigest: "missing",
	})
	if err == nil {
		t.Fatal("expected error for unknown tool spec")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("status got %v want NotFound", status.Code(err))
	}
}

func TestSubmitToolBuild_InvalidDockerfileRejectedBeforeBuildState(t *testing.T) {
	svc := newSubmitTestService(t)
	if err := svc.indexStore.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-invalid",
		ToolName:       "bwa-mem2",
		Version:        "2.2.1",
		RawSpec:        `{"tool_name":"bwa-mem2","version":"2.2.1","dockerfile_content":"FROM alpine:latest\nRUN true"}`,
		ResolvedAt:     time.Unix(101, 0).UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec invalid: %v", err)
	}

	_, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-invalid",
		ToolSpecDigest: "spec-invalid",
	})
	if err == nil {
		t.Fatal("expected invalid Dockerfile to be rejected")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("status got %v want InvalidArgument", status.Code(err))
	}
	if _, getErr := svc.buildState.Get("build-invalid"); !errors.Is(getErr, buildstate.ErrNotFound) {
		t.Fatalf("build state should not be created, got err %v", getErr)
	}
}

func TestWatchToolBuild_SendsCurrentState(t *testing.T) {
	svc := newSubmitTestService(t)
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-watch",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-watch"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	if len(stream.events) == 0 {
		t.Fatal("expected at least one state event")
	}
	last := stream.events[len(stream.events)-1]
	if last.GetBuildId() != "build-watch" || last.GetStatus() != string(buildstate.StatusSucceeded) {
		t.Fatalf("unexpected final event: %+v", last)
	}
}

// TestWatchToolBuild_ExposesImageDigest verifies AC-EVT-02: WatchToolBuild's
// final event must carry image_ref/image_digest without the caller having to
// parse logs or read index state directly.
func TestWatchToolBuild_ExposesImageDigest(t *testing.T) {
	svc := newSubmitTestService(t)
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-digest",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-digest"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	if len(stream.events) == 0 {
		t.Fatal("expected at least one state event")
	}
	last := stream.events[len(stream.events)-1]
	if last.GetStatus() != string(buildstate.StatusSucceeded) {
		t.Fatalf("final status got %q, want Succeeded", last.GetStatus())
	}
	if last.GetImageDigest() != "sha256:built" {
		t.Errorf("ImageDigest: got %q, want %q", last.GetImageDigest(), "sha256:built")
	}
	if last.GetImageRef() == "" {
		t.Error("ImageRef should be populated once the build has pushed")
	}
}

type cancelableBuilder struct {
	started chan struct{}
}

func (b *cancelableBuilder) Build(ctx context.Context, _, _ string) (imageID, digest string, err error) {
	close(b.started)
	<-ctx.Done()
	return "", "", ctx.Err()
}

func (*cancelableBuilder) Close() error { return nil }

func TestCancelToolBuild_MarksInterrupted(t *testing.T) {
	svc := newSubmitTestService(t)
	blocking := &cancelableBuilder{started: make(chan struct{})}
	svc.builder = blocking
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-cancel",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("submitted build did not reach builder")
	}

	resp, err := svc.CancelToolBuild(context.Background(), &nfv1.CancelToolBuildRequest{
		BuildId: "build-cancel",
		Reason:  "user requested",
	})
	if err != nil {
		t.Fatalf("CancelToolBuild: %v", err)
	}
	if resp.GetStatus() != string(buildstate.StatusInterrupted) {
		t.Fatalf("status got %q want Interrupted", resp.GetStatus())
	}
	got, err := svc.buildState.Get("build-cancel")
	if err != nil {
		t.Fatalf("buildState.Get: %v", err)
	}
	if got.Status != buildstate.StatusInterrupted {
		t.Fatalf("stored status got %q want Interrupted", got.Status)
	}
}

type subprocessBuilder struct {
	started chan struct{}
	exited  chan struct{}
}

// Build simulates a Builder whose underlying podbridge5/Buildah subprocess
// only stops once ctx is canceled; exited closing models the subprocess's
// kill/wait cleanup completing, not just Build returning.
func (b *subprocessBuilder) Build(ctx context.Context, _, _ string) (imageID, digest string, err error) {
	close(b.started)
	defer close(b.exited)
	<-ctx.Done()
	return "", "", ctx.Err()
}

func (*subprocessBuilder) Close() error { return nil }

func TestBuildCancel_CleansUpSubprocess(t *testing.T) {
	svc := newSubmitTestService(t)
	sub := &subprocessBuilder{started: make(chan struct{}), exited: make(chan struct{})}
	svc.builder = sub
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-subprocess-cancel",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	select {
	case <-sub.started:
	case <-time.After(time.Second):
		t.Fatal("submitted build did not reach builder")
	}

	svc.activeMu.Lock()
	_, tracked := svc.active["build-subprocess-cancel"]
	svc.activeMu.Unlock()
	if !tracked {
		t.Fatal("expected active build to be tracked before cancel")
	}

	if _, err := svc.CancelToolBuild(context.Background(), &nfv1.CancelToolBuildRequest{
		BuildId: "build-subprocess-cancel",
	}); err != nil {
		t.Fatalf("CancelToolBuild: %v", err)
	}

	select {
	case <-sub.exited:
	case <-time.After(time.Second):
		t.Fatal("builder did not clean up its subprocess after cancel")
	}

	deadline := time.Now().Add(time.Second)
	for {
		svc.activeMu.Lock()
		_, stillTracked := svc.active["build-subprocess-cancel"]
		svc.activeMu.Unlock()
		if !stillTracked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active build entry leaked after cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSubmitToolBuild_IdempotentRetry(t *testing.T) {
	svc := newSubmitTestService(t)
	first, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{RequestId: "build-retry", ToolSpecDigest: "spec-123"})
	if err != nil {
		t.Fatalf("first SubmitToolBuild: %v", err)
	}
	second, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{RequestId: "build-retry", ToolSpecDigest: "spec-123"})
	if err != nil {
		t.Fatalf("second SubmitToolBuild: %v", err)
	}
	if first.GetBuildId() != second.GetBuildId() {
		t.Fatalf("build ids differ: %q vs %q", first.GetBuildId(), second.GetBuildId())
	}
}
