package build

import (
	"context"
	"errors"
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

// fakeSentinel counts EnqueueValidationWork calls and records the last
// request, for asserting registration failure skips sentinel enqueue
// entirely (#23), that a successful registration enqueues exactly once with
// the correct fields, and that an injected enqueue error is non-fatal.
type fakeSentinel struct {
	calls   int
	lastReq *nsv1.EnqueueValidationWorkRequest
	err     error
}

func (f *fakeSentinel) EnqueueValidationWork(
	_ context.Context, req *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
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

// TestPostBuildRegistration_Success_EnqueuesSentinelWorkOnceWithCorrectFields
// is PR1's core wiring regression guard: once registration succeeds,
// EnqueueValidationWork must be called exactly once, and the request must
// carry the image repository/digest and the CasHash RegisterTool returned —
// not, e.g., a ToolSpec digest or some other identifier mixed in for CasHash
// (see the review finding that NodeSentinel's L5-a submission conflates
// CasHash and ToolSpecDigest downstream; this guards NodeVault's own side of
// that boundary).
func TestPostBuildRegistration_Success_EnqueuesSentinelWorkOnceWithCorrectFields(t *testing.T) {
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "127.0.0.1:1") // connection refused, fails fast (referrer push only)

	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	cat := catalog.NewCatalogAt(t.TempDir())
	registrySvc := catalog.NewToolRegistryService(cat, store)
	sentinel := &fakeSentinel{}
	svc := &Service{registry: registrySvc, indexStore: store, sentinel: sentinel}

	const imageDigest = "sha256:deadbeef"
	req := &nfv1.BuildRequest{RequestId: "req-1", ToolName: "test-tool", Version: "1.0.0"}
	// A tag reference, not a digest reference: primaryBuildDestination/
	// versionedDestination/latestDestination only ever produce "...:tag"
	// (Buildah pushes by tag, never by digest) — this must match what
	// production actually passes in here, not a shape that never occurs.
	destination := "harbor.example.com/library/test-tool:1.0.0"
	if regErr := svc.postBuildRegistration(context.Background(), req, destination, imageDigest, func(string) {}); regErr != nil {
		t.Fatalf("postBuildRegistration: %v (registration itself must succeed)", regErr)
	}

	if sentinel.calls != 1 {
		t.Fatalf("sentinel enqueue calls = %d, want exactly 1", sentinel.calls)
	}
	got := sentinel.lastReq
	if got == nil {
		t.Fatal("sentinel.lastReq is nil")
	}
	if got.ImageRepository != "harbor.example.com/library/test-tool" {
		t.Errorf("ImageRepository = %q, want the digest-stripped repo", got.ImageRepository)
	}
	if got.ImageDigest != imageDigest {
		t.Errorf("ImageDigest = %q, want %q", got.ImageDigest, imageDigest)
	}
	if got.ToolName != "test-tool" || got.Version != "1.0.0" {
		t.Errorf("ToolName/Version = %q/%q, want test-tool/1.0.0", got.ToolName, got.Version)
	}

	entries, lerr := store.All()
	if lerr != nil {
		t.Fatalf("store.All: %v", lerr)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 index entry, got %d", len(entries))
	}
	wantCasHash := entries[0].CasHash
	if wantCasHash == "" {
		t.Fatal("index entry has empty CasHash")
	}
	if got.CasHash != wantCasHash {
		t.Errorf("CasHash = %q, want the RegisterTool-assigned CasHash %q", got.CasHash, wantCasHash)
	}
}

// TestImageRepoFromDestination guards the exact bug the fixture above would
// have silently missed: destination is always a tag reference in production
// (Buildah pushes by tag, never by digest — see primaryBuildDestination),
// but the prior implementation only stripped an "@digest" suffix, leaving
// ":tag" glued onto ImageRepository for every real build.
func TestImageRepoFromDestination(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"tag", "harbor.example.com/library/bwa:0.7.17", "harbor.example.com/library/bwa"},
		{"digest", "harbor.example.com/library/bwa@sha256:" + strings.Repeat("a", 64), "harbor.example.com/library/bwa"},
		{"host_port_tag", "localhost:5000/library/bwa:0.7.17", "localhost:5000/library/bwa"},
		{"no_tag_or_digest", "harbor.example.com/library/bwa", "harbor.example.com/library/bwa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageRepoFromDestination(tc.ref); got != tc.want {
				t.Errorf("imageRepoFromDestination(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

// TestPostBuildRegistration_SentinelEnqueueFailure_IsNonFatal verifies that a
// NodeSentinel enqueue failure defers validation but does not fail
// postBuildRegistration — the image build, push, and catalog registration
// already succeeded by this point and must not be reported as failed.
func TestPostBuildRegistration_SentinelEnqueueFailure_IsNonFatal(t *testing.T) {
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "127.0.0.1:1") // connection refused, fails fast (referrer push only)

	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	cat := catalog.NewCatalogAt(t.TempDir())
	registrySvc := catalog.NewToolRegistryService(cat, store)
	sentinel := &fakeSentinel{err: errors.New("simulated NodeSentinel unavailable")}
	svc := &Service{registry: registrySvc, indexStore: store, sentinel: sentinel}

	var logs []string
	req := &nfv1.BuildRequest{RequestId: "req-1", ToolName: "test-tool", Version: "1.0.0"}
	regErr := svc.postBuildRegistration(context.Background(), req,
		"harbor.example.com/library/test-tool:1.0.0", "sha256:deadbeef",
		func(msg string) { logs = append(logs, msg) })
	if regErr != nil {
		t.Fatalf("postBuildRegistration: %v, want nil (sentinel enqueue failure must not fail registration)", regErr)
	}
	if sentinel.calls != 1 {
		t.Fatalf("sentinel enqueue calls = %d, want 1", sentinel.calls)
	}

	foundFailureLog := false
	for _, l := range logs {
		if strings.Contains(l, "sentinel enqueue failed") {
			foundFailureLog = true
		}
	}
	if !foundFailureLog {
		t.Errorf("expected a %q log line, got %v", "sentinel enqueue failed", logs)
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
//
// See TestSubmitToolBuild_DisabledService_ReturnsUnavailable
// (submit_tool_build_test.go) for disabled-backend coverage against the
// current API — NewDisabledService has no indexStore/buildState wired, so
// SubmitToolBuild rejects with Unavailable before ever reaching the builder.

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

// TestSanitizeTag_LongSafeStringsDoNotCollideOnTruncation guards a bypass of
// the collision guarantee that wasn't going through character substitution:
// two already-tag-safe strings sharing the same first tagMaxLen characters
// used to both truncate to the identical tag, silently conflating two
// different versions — exactly what the hash-suffix mechanism exists to
// prevent for the character-substitution case.
func TestSanitizeTag_LongSafeStringsDoNotCollideOnTruncation(t *testing.T) {
	base := strings.Repeat("a", tagMaxLen)
	first := base + "x"
	second := base + "y"

	gotFirst := sanitizeTag(first)
	gotSecond := sanitizeTag(second)
	if gotFirst == gotSecond {
		t.Fatalf("sanitizeTag collision: %q and %q both produced %q", first, second, gotFirst)
	}
	if len(gotFirst) <= tagMaxLen || len(gotSecond) <= tagMaxLen {
		t.Errorf("expected a hash suffix appended past tagMaxLen, got %q and %q", gotFirst, gotSecond)
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
