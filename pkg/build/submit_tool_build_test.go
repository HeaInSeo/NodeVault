package build

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// TestBuildRequestFromResolved_TrailingContent_Rejected guards against
// json.Decoder only reading the first JSON value in a stream: a raw_spec
// with trailing content after a well-formed first object must be rejected
// rather than silently decoding just the first value and ignoring the rest.
func TestBuildRequestFromResolved_TrailingContent_Rejected(t *testing.T) {
	spec := index.ResolvedToolSpec{
		ToolSpecDigest: "spec-trailing-1",
		ToolName:       "bwa",
		RawSpec: `{"tool_name":"bwa","dockerfile_content":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"}` +
			` {"junk":"trailing"}`,
	}
	_, err := buildRequestFromResolved("build-trailing-1", spec)
	if err == nil {
		t.Fatal("buildRequestFromResolved: expected an error for trailing content after the first JSON value, got nil")
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

func (b *cancelableBuilder) Build(
	ctx context.Context, _, _ string,
) (imageID, digest string, layerCacheHit bool, err error) {
	close(b.started)
	<-ctx.Done()
	return "", "", false, ctx.Err()
}

func (*cancelableBuilder) PushTag(_ context.Context, _, _ string) (digest string, err error) {
	return "", nil
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
func (b *subprocessBuilder) Build(
	ctx context.Context, _, _ string,
) (imageID, digest string, layerCacheHit bool, err error) {
	close(b.started)
	defer close(b.exited)
	<-ctx.Done()
	return "", "", false, ctx.Err()
}

func (*subprocessBuilder) PushTag(_ context.Context, _, _ string) (digest string, err error) {
	return "", nil
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
	buildCalls := 0
	svc.builder = &countCallBuilder{inner: svc.builder, calls: &buildCalls}

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

	if err := svc.WatchToolBuild(
		&nfv1.WatchToolBuildRequest{BuildId: first.GetBuildId()},
		newFakeStream(),
	); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	if buildCalls != 1 {
		t.Fatalf("Build called %d times, want exactly 1 for an idempotent retry", buildCalls)
	}
}

func TestSubmitToolBuild_RequestIDConflictRejected(t *testing.T) {
	svc := newSubmitTestService(t)
	if err := svc.indexStore.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-456",
		ToolName:       "samtools",
		Version:        "1.20",
		RawSpec:        `{"tool_name":"samtools","version":"1.20","dockerfile_content":"FROM alpine:3.20@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\\nRUN true"}`,
		ResolvedAt:     time.Unix(200, 0).UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-conflict",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("first SubmitToolBuild: %v", err)
	}
	_, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-conflict",
		ToolSpecDigest: "spec-456",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflicting SubmitToolBuild error = %v, want codes.AlreadyExists", err)
	}

	rec, getErr := svc.buildState.Get("build-conflict")
	if getErr != nil {
		t.Fatalf("buildState.Get: %v", getErr)
	}
	if rec.ToolSpecDigest != "spec-123" {
		t.Fatalf("stored tool spec digest = %q, want original spec-123", rec.ToolSpecDigest)
	}
}

// TestAbandonSubmittedBuild_RecordsDurableFailure guards against a
// buildstate.Transition failure leaving a build in a ghost state with no
// record anywhere: even though buildstate's own record is stuck (the same
// store that just failed to write), pkg/index — a separate store — must
// still end up with a failure record for the build.
func TestAbandonSubmittedBuild_RecordsDurableFailure(t *testing.T) {
	svc := newSubmitTestService(t)
	rec := buildstate.Record{BuildID: "build-abandoned-1", RequestedAt: time.Now().UTC()}

	svc.abandonSubmittedBuild(rec, "resolving", errors.New("simulated buildstate write failure"))

	got, err := svc.indexStore.GetToolBuildRecordByBuildID("build-abandoned-1")
	if err != nil {
		t.Fatalf("GetToolBuildRecordByBuildID: %v", err)
	}
	if got.Success {
		t.Error("ToolBuildRecord.Success = true, want false")
	}
	if got.FailureReason == "" {
		t.Error("ToolBuildRecord.FailureReason is empty, want a reason mentioning the stage and underlying error")
	}
}

// ─── issue #26: WatchToolBuild must not hang forever when buildstate writes fail ───
//
// These simulate a buildstate.Transition write failure directly (the same
// technique TestAbandonSubmittedBuild_RecordsDurableFailure uses) rather
// than injecting a real SQLite fault, then drive WatchToolBuild's real poll
// loop against it. watchPollInterval is 100ms, so a correct fix must
// terminate these within a couple of poll ticks — a regression back to the
// old unconditional "if unchanged, keep waiting" behavior would hang until
// the test's own timeout instead.

func newAbandonableBuild(t *testing.T, svc *Service, buildID string) buildstate.Record {
	t.Helper()
	rec, _, err := svc.buildState.CreateOrGet(buildID, "spec-123", time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateOrGet: %v", err)
	}
	if _, err := svc.buildState.Transition(buildID, buildstate.StatusBuilding, "", time.Now().UTC()); err != nil {
		t.Fatalf("Transition to Building: %v", err)
	}
	entry := &activeBuild{cancel: func() {}, done: make(chan struct{})}
	svc.activeMu.Lock()
	if svc.active == nil {
		svc.active = make(map[string]*activeBuild)
	}
	svc.active[buildID] = entry
	svc.activeMu.Unlock()
	rec.Status = buildstate.StatusBuilding
	return rec
}

func watchAsync(svc *Service, buildID string) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: buildID}, newFakeStream())
	}()
	return result
}

func TestWatchToolBuild_AbandonedWhileWatcherWaiting_TerminatesWithInternalError(t *testing.T) {
	svc := newSubmitTestService(t)
	rec := newAbandonableBuild(t, svc, "build-abandon-watching")

	result := watchAsync(svc, "build-abandon-watching")
	// Give the watcher time to reach its poll loop before abandoning, so
	// this genuinely exercises "already waiting" rather than racing it.
	time.Sleep(20 * time.Millisecond)
	svc.abandonSubmittedBuild(rec, "building", errors.New("simulated buildstate write failure"))

	select {
	case err := <-result:
		if status.Code(err) != codes.Internal {
			t.Fatalf("WatchToolBuild error got %v, want codes.Internal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchToolBuild did not terminate after the build was abandoned")
	}
}

func TestWatchToolBuild_LateConnectAfterAbandon_TerminatesWithInternalError(t *testing.T) {
	svc := newSubmitTestService(t)
	rec := newAbandonableBuild(t, svc, "build-abandon-late")
	svc.abandonSubmittedBuild(rec, "building", errors.New("simulated buildstate write failure"))

	result := watchAsync(svc, "build-abandon-late")
	select {
	case err := <-result:
		if status.Code(err) != codes.Internal {
			t.Fatalf("WatchToolBuild error got %v, want codes.Internal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchToolBuild did not terminate for a build already abandoned before it connected")
	}
}

func TestWatchToolBuild_MultipleWatchers_AllTerminateOnAbandon(t *testing.T) {
	svc := newSubmitTestService(t)
	rec := newAbandonableBuild(t, svc, "build-abandon-multi")

	watcherA := watchAsync(svc, "build-abandon-multi")
	watcherB := watchAsync(svc, "build-abandon-multi")
	time.Sleep(20 * time.Millisecond)
	svc.abandonSubmittedBuild(rec, "building", errors.New("simulated buildstate write failure"))

	for name, ch := range map[string]<-chan error{"A": watcherA, "B": watcherB} {
		select {
		case err := <-ch:
			if status.Code(err) != codes.Internal {
				t.Fatalf("watcher %s error got %v, want codes.Internal", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher %s did not terminate after the build was abandoned", name)
		}
	}
}

func TestFinalizeSubmittedBuild_SuccessRemovesActiveEntry(t *testing.T) {
	svc := newSubmitTestService(t)
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-finalize-success",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	// The mock builder completes synchronously-ish; poll briefly for the
	// background goroutine to reach its terminal transition.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		svc.activeMu.Lock()
		_, tracked := svc.active["build-finalize-success"]
		svc.activeMu.Unlock()
		if !tracked {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	svc.activeMu.Lock()
	_, tracked := svc.active["build-finalize-success"]
	svc.activeMu.Unlock()
	if tracked {
		t.Error("active entry should be removed once the build reaches a durable terminal state")
	}
	got, err := svc.buildState.Get("build-finalize-success")
	if err != nil {
		t.Fatalf("buildState.Get: %v", err)
	}
	if got.Status != buildstate.StatusSucceeded {
		t.Fatalf("status got %q, want Succeeded", got.Status)
	}
}

func TestActiveBuildFail_ClosesDoneAtMostOnce(t *testing.T) {
	entry := &activeBuild{cancel: func() {}, done: make(chan struct{})}
	firstErr := errors.New("first failure")
	secondErr := errors.New("second failure")

	entry.fail(firstErr)
	entry.fail(secondErr) // must not panic on double-close, must not overwrite err

	select {
	case <-entry.done:
	default:
		t.Fatal("done should be closed after fail")
	}
	if !errors.Is(entry.err, firstErr) {
		t.Fatalf("err got %v, want the first failure to win", entry.err)
	}
}

// ─── SubmitToolBuild-path coverage for behaviors formerly tested only via the
// now-removed legacy BuildAndRegister RPC (issue #15) ─────────────────────────
//
// These preserve the same assertions the deleted TestBuildAndRegister_* tests
// made, adapted to the SubmitToolBuild/WatchToolBuild terminal-event model —
// runSubmittedBuild shares the exact same builder.Build call, recordBuildFailure,
// pushLatestAlias, and warnIfTagReassigned functions BuildAndRegister used, so
// this is the same underlying behavior observed through the current API.

// TestSubmitToolBuild_BuilderErrorNoRetry_TransitionsToFailed verifies that a
// build error (including a rootless/user-namespace failure) causes exactly one
// Build call (no privileged-fallback retry — NodeVault has no such logic) and
// a Failed terminal state, never Succeeded.
func TestSubmitToolBuild_BuilderErrorNoRetry_TransitionsToFailed(t *testing.T) {
	svc := newSubmitTestService(t)
	buildCalls := 0
	rootlessErr := fmt.Errorf(
		"build image: error building at STEP 2: error processing " +
			"RUN mknod /dev/test c 1 3: exit status 1: mknod: /dev/test: Operation not permitted",
	)
	svc.builder = &countCallBuilder{inner: &mockBuilder{err: rootlessErr}, calls: &buildCalls}

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-builder-err", ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-builder-err"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	last := stream.events[len(stream.events)-1]
	if last.GetStatus() != string(buildstate.StatusFailed) {
		t.Fatalf("final status = %q, want Failed", last.GetStatus())
	}
	if buildCalls != 1 {
		t.Errorf("Build called %d times, want exactly 1 — no privileged retry", buildCalls)
	}
}

// TestSubmitToolBuild_BuilderError_RecordsFailedToolBuildRecord verifies a
// build failure is durably recorded in the index with Success=false.
func TestSubmitToolBuild_BuilderError_RecordsFailedToolBuildRecord(t *testing.T) {
	svc := newSubmitTestService(t)
	svc.builder = &mockBuilder{err: fmt.Errorf("image build backend: exec format error")}

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-record-fail", ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-record-fail"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}

	got, err := svc.indexStore.GetToolBuildRecordByBuildID("build-record-fail")
	if err != nil {
		t.Fatalf("GetToolBuildRecordByBuildID: %v", err)
	}
	if got.Success {
		t.Error("ToolBuildRecord.Success = true, want false")
	}
	if got.FailureReason == "" {
		t.Error("ToolBuildRecord.FailureReason is empty")
	}
}

// TestSubmitToolBuild_DisabledService_ReturnsUnavailable verifies spike mode
// (NODEVAULT_BUILD_BACKEND=disabled, NewDisabledService) rejects a submit
// cleanly instead of reaching a nil builder — the server itself stays alive,
// only this RPC fails.
func TestSubmitToolBuild_DisabledService_ReturnsUnavailable(t *testing.T) {
	svc := NewDisabledService()
	_, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-disabled-1", ToolSpecDigest: "spec-123",
	})
	if err == nil {
		t.Fatal("expected an error from a disabled build backend, got nil")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("status got %v, want Unavailable", status.Code(err))
	}
}

// TestSubmitToolBuild_VersionedBuild_PushesLatestAlias verifies the primary
// build/push targets the version-pinned tag, and :latest is pushed
// separately as a best-effort alias.
func TestSubmitToolBuild_VersionedBuild_PushesLatestAlias(t *testing.T) {
	svc := newSubmitTestService(t)
	builder := &mockBuilder{digest: "sha256:versioned"}
	svc.builder = builder
	if err := svc.indexStore.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-versioned", ToolName: "bwa-mem2", Version: "0.7.17",
		RawSpec:    `{"tool_name":"bwa-mem2","version":"0.7.17","dockerfile_content":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"}`,
		ResolvedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-ver-1", ToolSpecDigest: "spec-versioned",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-ver-1"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	last := stream.events[len(stream.events)-1]
	if last.GetStatus() != string(buildstate.StatusSucceeded) {
		t.Fatalf("final status = %q, want Succeeded", last.GetStatus())
	}
	if !strings.Contains(last.GetImageRef(), ":0.7.17") {
		t.Errorf("ImageRef = %q, want the version-pinned destination", last.GetImageRef())
	}
	if len(builder.pushTagCalls) != 1 || !strings.HasSuffix(builder.pushTagCalls[0], ":latest") {
		t.Errorf("PushTag calls = %v, want exactly one call to a :latest destination", builder.pushTagCalls)
	}
}

// TestSubmitToolBuild_NoVersion_NoLatestAliasPush verifies that when no
// version is available, Build's primary destination is already :latest, so
// no separate PushTag call happens.
func TestSubmitToolBuild_NoVersion_NoLatestAliasPush(t *testing.T) {
	svc := newSubmitTestService(t)
	builder := &mockBuilder{digest: "sha256:novers"}
	svc.builder = builder
	// newSubmitTestService's default "spec-123" fixture already carries
	// Version "2.2.1" (see its ResolvedToolSpec) — register a spec with no
	// version so this test actually exercises the no-version path.
	if err := svc.indexStore.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-novers", ToolName: "bwa-mem2",
		RawSpec:    `{"tool_name":"bwa-mem2","dockerfile_content":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"}`,
		ResolvedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-novers-1", ToolSpecDigest: "spec-novers",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-novers-1"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	if len(builder.pushTagCalls) != 0 {
		t.Errorf("PushTag calls = %v, want none", builder.pushTagCalls)
	}
}

// TestSubmitToolBuild_LatestAliasPushFails_BuildStillSucceeds verifies a
// :latest alias push failure does not fail the build — the version-pinned
// primary destination already succeeded, and latest is a best-effort
// convenience pointer, not the authoritative reference.
func TestSubmitToolBuild_LatestAliasPushFails_BuildStillSucceeds(t *testing.T) {
	svc := newSubmitTestService(t)
	builder := &mockBuilder{digest: "sha256:aliasfail", pushTagErr: errors.New("registry unavailable")}
	svc.builder = builder
	if err := svc.indexStore.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-aliasfail", ToolName: "bwa-mem2", Version: "0.7.17",
		RawSpec:    `{"tool_name":"bwa-mem2","version":"0.7.17","dockerfile_content":"FROM alpine:3.20@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nRUN true"}`,
		ResolvedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}

	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId: "build-aliasfail-1", ToolSpecDigest: "spec-aliasfail",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-aliasfail-1"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	last := stream.events[len(stream.events)-1]
	if last.GetStatus() != string(buildstate.StatusSucceeded) {
		t.Fatalf("final status = %q, want Succeeded despite latest alias push failure", last.GetStatus())
	}
}
