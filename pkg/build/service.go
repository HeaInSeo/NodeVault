// Package build manages builder orchestration via podbridge5.
// BuildService receives BuildRequests, calls podbridge5 to build and push images,
// streams events back to the caller, and acquires the pushed image digest.
// After L2 succeeds it drives L3 (dry-run) → L4 (smoke run) → tool registration.
package build

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
	"github.com/HeaInSeo/NodeVault/pkg/oras"
	"github.com/HeaInSeo/NodeVault/pkg/validate"
)

// SentinelEnqueuer enqueues post-build L3/L4 validation work with NodeSentinel.
// Implemented by *sentinelclient.Client in production; nil disables enqueueing
// (e.g. when NodeSentinel is not deployed yet).
type SentinelEnqueuer interface {
	EnqueueValidationWork(
		ctx context.Context, req *nsv1.EnqueueValidationWorkRequest,
	) (*nsv1.EnqueueValidationWorkResponse, error)
}

// ReconcileTriggerer triggers a targeted integrity check for one artifact.
// Implemented by *reconcile.Reconciler in production; nil disables eager reconcile.
// Authority: only SetIntegrityHealth is called through this path (reconcile axis).
type ReconcileTriggerer interface {
	ReconcileOne(ctx context.Context, casHash string) error
}

const (
	defaultRegistryAddr = "harbor.10.113.24.96.nip.io"
	backendInPodBuildah = "in-pod-buildah"
)

func registryAddr() string {
	if v := os.Getenv("NODEVAULT_REGISTRY_ADDR"); v != "" {
		return v
	}
	return defaultRegistryAddr
}

// Service implements BuildServiceServer.
type Service struct {
	nfv1.UnimplementedBuildServiceServer
	builder           Builder
	validator         *validate.Service
	registry          *catalog.ToolRegistryService
	indexStore        *index.Store
	buildState        *buildstate.Store
	activeMu          sync.Mutex
	active            map[string]context.CancelFunc
	reconciler        ReconcileTriggerer // nil = no eager reconcile
	sentinel          SentinelEnqueuer   // nil = no L3/L4 enqueue
	baseImageResolver baseImageResolver  // nil = lazily uses registry.NewClient()
}

// NewService creates a BuildService backed by podbridge5.
// reconciler may be nil; when non-nil it is called after successful referrer push
// so integrity_health transitions to Healthy without waiting for the next reconcile tick.
func NewService(
	validator *validate.Service,
	registry *catalog.ToolRegistryService,
	store *index.Store,
	stateStore *buildstate.Store,
	reconciler ReconcileTriggerer,
) (*Service, error) {
	if validator == nil {
		return nil, fmt.Errorf("build service init: validator must not be nil")
	}
	if registry == nil {
		return nil, fmt.Errorf("build service init: registry must not be nil")
	}
	if store == nil {
		return nil, fmt.Errorf("build service init: index store must not be nil")
	}
	if stateStore == nil {
		return nil, fmt.Errorf("build service init: build state store must not be nil")
	}

	builder, err := newPodbridge5Builder()
	if err != nil {
		return nil, fmt.Errorf("build service init: %w", err)
	}
	return &Service{
		builder: builder, validator: validator, registry: registry,
		indexStore: store, buildState: stateStore, active: make(map[string]context.CancelFunc), reconciler: reconciler,
	}, nil
}

// NewDisabledService creates a BuildService that immediately rejects all build
// requests with ErrBuildBackendDisabled. The gRPC server stays alive; other
// services (Ping, Policy, Catalog) continue to function normally.
// Used when NODEVAULT_BUILD_BACKEND=disabled (incluster spike mode).
func NewDisabledService() *Service {
	return &Service{builder: disabledBuilder{}}
}

// Close releases the underlying image build storage.
func (s *Service) Close() error {
	s.activeMu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.activeMu.Unlock()
	if s.builder == nil {
		return nil
	}
	return s.builder.Close()
}

// BuildAndRegister implements BuildServiceServer.
// Full orchestration: L2 (image build+push) → L3 (dry-run) → L4 (smoke run) → registration.
//
//nolint:funlen // orchestration function — extracting sub-steps would obscure the L2→L3→L4 sequence.
func (s *Service) BuildAndRegister(req *nfv1.BuildRequest, stream grpc.ServerStreamingServer[nfv1.BuildEvent]) error {
	ctx := stream.Context()

	send := func(kind nfv1.BuildEventKind, msg string) error {
		return stream.Send(&nfv1.BuildEvent{
			Kind:      kind,
			Message:   msg,
			Timestamp: time.Now().UnixMilli(),
		})
	}

	destination := fmt.Sprintf("%s/library/%s:latest", registryAddr(), sanitizeName(req.ToolName))

	// buildID identifies this single build execution for ToolBuildRecord/ToolImageRecord.
	// Derived from RequestId + start time to reduce collision risk across retries that
	// reuse the same RequestId (see risk note in service.go doc comment).
	buildStartedAt := time.Now().UTC()
	buildID := fmt.Sprintf("%s-%d", req.RequestId, buildStartedAt.UnixNano())

	// ── L2: image build + push ───────────────────────────────────────────────────

	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_JOB_CREATED, "image build starting: "+destination)
	slog.Info("image build starting", "destination", destination)

	_, digest, err := s.builder.Build(ctx, req.DockerfileContent, destination)
	if err != nil {
		metrics.BuildFailureTotal.Add(1)
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED, err.Error())
		s.recordBuildFailure(buildID, buildStartedAt, err)
		return fmt.Errorf("image build: %w", err)
	}

	s.recordBuildSuccess(buildID, buildStartedAt, digest, destination)
	metrics.BuildSuccessTotal.Add(1)
	slog.Info("image build succeeded", "destination", destination, "digest", digest)
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_PUSH_SUCCEEDED, "image pushed to "+destination)

	imageWithDigest := destination + "@" + digest
	_ = stream.Send(&nfv1.BuildEvent{
		Kind:      nfv1.BuildEventKind_BUILD_EVENT_KIND_DIGEST_ACQUIRED,
		Message:   imageWithDigest,
		Digest:    digest,
		Timestamp: time.Now().UnixMilli(),
	})

	// ── L3: dry-run ──────────────────────────────────────────────────────────────

	reqID := req.RequestId
	if len(reqID) > 8 {
		reqID = reqID[:8]
	}
	jobSuffix := sanitizeName(reqID)
	if jobSuffix == "" {
		jobSuffix = fmt.Sprintf("%04x", time.Now().UnixMilli()%0xFFFF)
	}
	smokeJob := validate.SmokeJobSpec("nfsmoke-"+jobSuffix, imageWithDigest)
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L3: submitting dry-run...")

	dryResult := s.validator.DryRunJob(ctx, smokeJob)
	if !dryResult.Success {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED, "L3 dry-run failed: "+dryResult.ErrorMessage)
		return fmt.Errorf("L3 dry-run failed: %s", dryResult.ErrorMessage)
	}
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L3 dry-run passed")

	// ── L4: smoke run ────────────────────────────────────────────────────────────

	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L4: starting smoke run...")

	smokeResult := s.validator.SmokeRunJob(ctx, smokeJob)
	if !smokeResult.Success {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED, "L4 smoke run failed: "+smokeResult.ErrorMessage)
		return fmt.Errorf("L4 smoke run failed: %s", smokeResult.ErrorMessage)
	}
	if smokeResult.LogOutput != "" {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "smoke log: "+strings.TrimSpace(smokeResult.LogOutput))
	}
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L4 smoke run passed")

	// ── 등록 + spec referrer + NodeSentinel ──────────────────────────────────────
	logSend := func(msg string) { _ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, msg) }
	s.postBuildRegistration(ctx, req, destination, digest, logSend)

	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED,
		fmt.Sprintf("build+register complete: %s@%s", destination, digest))
	return nil
}

// recordBuildFailure persists a failed ToolBuildRecord to the index, if a Store is wired.
// Best-effort: a recording failure is logged but never fails the RPC.
func (s *Service) recordBuildFailure(buildID string, startedAt time.Time, buildErr error) {
	if s.indexStore == nil {
		return
	}
	rec := index.ToolBuildRecord{
		BuildID:       buildID,
		Backend:       s.builderBackendName(),
		Execution:     s.buildExecution(),
		StartedAt:     startedAt,
		CompletedAt:   time.Now().UTC(),
		Success:       false,
		FailureReason: buildErr.Error(),
	}
	if err := s.indexStore.AppendToolBuildRecord(rec); err != nil {
		slog.Warn("index: failed to record failed ToolBuildRecord", "build_id", buildID, "err", err)
	}
}

// recordBuildSuccess persists a successful ToolBuildRecord and the corresponding
// ToolImageRecord to the index, if a Store is wired. Best-effort: a recording
// failure is logged but never fails the RPC — the image has already been pushed.
func (s *Service) recordBuildSuccess(buildID string, startedAt time.Time, digest, imageRef string) {
	if s.indexStore == nil {
		return
	}
	completedAt := time.Now().UTC()
	rec := index.ToolBuildRecord{
		BuildID:     buildID,
		ImageDigest: digest,
		Backend:     s.builderBackendName(),
		Execution:   s.buildExecution(),
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		Success:     true,
	}
	if err := s.indexStore.AppendToolBuildRecord(rec); err != nil {
		slog.Warn("index: failed to record successful ToolBuildRecord", "build_id", buildID, "err", err)
	}

	img := index.ToolImageRecord{
		ImageDigest: digest,
		ImageRef:    imageRef,
		BuildID:     buildID,
		PushedAt:    completedAt,
	}
	if err := s.indexStore.AppendToolImageRecord(img); err != nil {
		slog.Warn("index: failed to record ToolImageRecord", "build_id", buildID, "image_digest", digest, "err", err)
	}
}

// postBuildRegistration performs tool registration, spec referrer push, and
// NodeSentinel enqueueing after a successful L2 image build. It is shared by
// BuildAndRegister (streaming RPC) and runSubmittedBuild (async goroutine).
// logFn receives informational messages for the caller's output channel; it
// must not be nil.
func (s *Service) postBuildRegistration(
	ctx context.Context,
	req *nfv1.BuildRequest,
	destination, digest string,
	logFn func(string),
) {
	if s.registry == nil {
		logFn("registration skipped: no registry configured")
		return
	}
	regResp, regErr := s.registry.RegisterTool(ctx, &nfv1.RegisterToolRequest{
		RequestId:        req.GetRequestId(),
		ToolDefinitionId: req.GetToolDefinitionId(),
		ToolName:         req.GetToolName(),
		ImageUri:         destination,
		Digest:           digest,
		EnvironmentSpec:  req.GetEnvironmentSpec(),
		Version:          req.GetVersion(),
		BuildKind:        req.GetKind(),
	})
	if regErr != nil {
		logFn("registration warning: " + regErr.Error())
	} else {
		logFn("tool registered: cas=" + regResp.CasHash)
	}

	// spec referrer push — non-fatal; integrity_health reconcile retries on failure.
	// integrity_health is updated ONLY via ReconcileOne (reconcile axis — authority map).
	if regErr == nil && s.indexStore != nil {
		imageRepo := fmt.Sprintf("%s/library/%s", registryAddr(), sanitizeName(req.GetToolName()))
		referrerDigest, refErr := oras.PushToolSpecReferrer(ctx, imageRepo, digest, regResp.Tool)
		if refErr != nil {
			slog.Warn("spec referrer push failed (integrity_health=Partial)", "err", refErr)
			logFn("spec referrer push failed: " + refErr.Error())
		} else {
			slog.Info("spec referrer attached", "referrer", referrerDigest)
			logFn("spec referrer attached: " + referrerDigest)
			if idxErr := s.indexStore.SetSpecReferrerDigest(regResp.CasHash, referrerDigest); idxErr != nil {
				slog.Warn("index spec referrer digest update failed", "err", idxErr)
			}
			if s.reconciler != nil {
				if recErr := s.reconciler.ReconcileOne(ctx, regResp.CasHash); recErr != nil {
					slog.Warn("eager reconcile after referrer push failed", "err", recErr)
				}
			}
		}
	}

	// NodeSentinel enqueue — non-fatal; validation is deferred if enqueue fails.
	if s.sentinel != nil {
		casHash := ""
		if regErr == nil {
			casHash = regResp.CasHash
		}
		imageRepo := destination
		if idx := strings.LastIndex(destination, "@"); idx != -1 {
			imageRepo = destination[:idx]
		}
		enqReq := &nsv1.EnqueueValidationWorkRequest{
			ArtifactKind:     "tool",
			ImageRepository:  imageRepo,
			ImageDigest:      digest,
			ToolName:         req.GetToolName(),
			Version:          req.GetVersion(),
			CasHash:          casHash,
			RequestedActions: []string{"smoke_run"},
		}
		enqCtx, enqCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer enqCancel()
		if enqResp, enqErr := s.sentinel.EnqueueValidationWork(enqCtx, enqReq); enqErr != nil {
			slog.Warn("NodeSentinel EnqueueValidationWork failed (validation deferred)", "err", enqErr)
			logFn("sentinel enqueue failed: " + enqErr.Error())
		} else {
			slog.Info("NodeSentinel job enqueued", "job_id", enqResp.JobId, "status", enqResp.Status)
			logFn("sentinel job enqueued: " + enqResp.JobId)
		}
	}
}

func (s *Service) buildExecution() *index.BuildExecution {
	if s.builderBackendName() != backendInPodBuildah {
		return nil
	}
	hostUsers := false
	exec := &index.BuildExecution{
		Mode: backendInPodBuildah, HostUsers: &hostUsers,
		StorageDriver: "overlay", Isolation: "chroot",
	}
	if ref := layerCacheRef(); ref != "" {
		exec.CacheRef = ref
	}
	return exec
}

// builderBackendName returns a human-readable identifier of the active build backend,
// used to populate ToolBuildRecord.Backend.
func (s *Service) builderBackendName() string {
	switch s.builder.(type) {
	case disabledBuilder:
		return "disabled"
	default:
		return backendInPodBuildah
	}
}

// sanitizeName makes a string safe for use as an image name component.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > 50 {
		result = result[:50]
	}
	return result
}
