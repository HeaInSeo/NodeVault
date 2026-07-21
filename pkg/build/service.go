// Package build manages builder orchestration via podbridge5.
// BuildService receives BuildRequests, calls podbridge5 to build and push images,
// streams events back to the caller, and acquires the pushed image digest.
// After L2 succeeds it drives L3 (dry-run) → L4 (smoke run) → tool registration.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/oras"
	"github.com/HeaInSeo/NodeVault/pkg/registryconfig"
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

const backendInPodBuildah = "in-pod-buildah"

func registryAddr() string {
	return registryconfig.FromEnv().Addr
}

// Service implements BuildServiceServer.
type Service struct {
	nfv1.UnimplementedBuildServiceServer
	builder           Builder
	registry          *catalog.ToolRegistryService
	indexStore        *index.Store
	buildState        *buildstate.Store
	activeMu          sync.Mutex
	active            map[string]*activeBuild
	reconciler        ReconcileTriggerer // nil = no eager reconcile
	sentinel          SentinelEnqueuer   // nil = no L3/L4 enqueue
	baseImageResolver baseImageResolver  // nil = lazily uses registry.NewClient()
}

// activeBuild tracks one in-flight runSubmittedBuild goroutine: cancel stops
// it (CancelToolBuild), and done/err let WatchToolBuild learn that the
// goroutine gave up because a buildstate write itself failed — the one
// failure mode buildstate's own durable record can never reveal, since it's
// the write that failed. done is closed at most once (via once) so multiple
// concurrent WatchToolBuild callers can all observe it.
type activeBuild struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	once   sync.Once
}

// fail records err and closes done, waking any WatchToolBuild callers
// selecting on it. Safe to call more than once; only the first call has an
// effect.
func (b *activeBuild) fail(err error) {
	b.once.Do(func() {
		b.err = err
		close(b.done)
	})
}

// NewService creates a BuildService backed by podbridge5.
// reconciler may be nil; when non-nil it is called after successful referrer push
// so integrity_health transitions to Healthy without waiting for the next reconcile tick.
func NewService(
	registry *catalog.ToolRegistryService,
	store *index.Store,
	stateStore *buildstate.Store,
	reconciler ReconcileTriggerer,
) (*Service, error) {
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
		builder: builder, registry: registry,
		indexStore: store, buildState: stateStore, active: make(map[string]*activeBuild), reconciler: reconciler,
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
	for _, entry := range s.active {
		entry.cancel()
	}
	s.activeMu.Unlock()
	if s.builder == nil {
		return nil
	}
	return s.builder.Close()
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
		Execution:     s.buildExecution(false),
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
func (s *Service) recordBuildSuccess(buildID string, startedAt time.Time, digest, imageRef string, layerCacheHit bool) {
	if s.indexStore == nil {
		return
	}
	completedAt := time.Now().UTC()
	rec := index.ToolBuildRecord{
		BuildID:     buildID,
		ImageDigest: digest,
		Backend:     s.builderBackendName(),
		Execution:   s.buildExecution(layerCacheHit),
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
// NodeSentinel enqueueing after a successful L2 image build. Called from
// runSubmittedBuild (async goroutine). logFn receives informational messages
// for the caller's output channel; it must not be nil.
//
// Returns an error only for a genuine RegisterTool RPC failure: a pushed
// image that never became a catalog entry isn't a usable tool, so the whole
// operation must be treated as failed even though the image build+push
// itself succeeded (see #23) — callers must not report SUCCEEDED in that
// case. destination/digest are already durable by this point (index
// ToolBuildRecord/ToolImageRecord via recordBuildSuccess, and for
// SubmitToolBuild, buildstate.SetArtifact), so nothing about the completed
// build/push is lost by surfacing this as a failure.
//
// s.registry == nil is a construction-time invariant NewService already
// enforces (never nil in production — see its own nil check); it is not
// treated as a failure here so unit tests that don't need a full registry
// mock keep working.
func (s *Service) postBuildRegistration(
	ctx context.Context,
	req *nfv1.BuildRequest,
	destination, digest string,
	logFn func(string),
) error {
	if s.registry == nil {
		logFn("registration skipped: no registry configured")
		return nil
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
		return fmt.Errorf("register tool: %w", regErr)
	}
	logFn("tool registered: cas=" + regResp.CasHash)

	// spec referrer push — non-fatal; integrity_health reconcile retries on failure.
	// integrity_health is updated ONLY via ReconcileOne (reconcile axis — authority map).
	if s.indexStore != nil {
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
		}
		// AC-REG-04: trigger reconcile regardless of referrer push outcome so
		// integrity_health reflects reality quickly either way — on success it
		// converges toward Healthy, on failure toward Partial. This only
		// recomputes the reconcile axis; lifecycle_phase (already set Active by
		// RegisterTool above) is untouched here.
		integrityHealth := ""
		if s.reconciler != nil {
			if recErr := s.reconciler.ReconcileOne(ctx, regResp.CasHash); recErr != nil {
				slog.Warn("eager reconcile after referrer push failed", "err", recErr)
			}
		}
		// Read-through snapshot for the buildstate bridge (Sprint 7, AC-EVT-02) —
		// this reads the value ReconcileOne just computed; it does not compute or
		// write integrity_health itself.
		if entry, getErr := s.indexStore.GetByCasHash(regResp.CasHash); getErr == nil {
			integrityHealth = string(entry.IntegrityHealth)
		}
		// req.GetRequestId() equals the buildstate build_id (buildRequestFromResolved
		// sets req.RequestId to the build_id). ErrNotFound is tolerated defensively
		// rather than logged as a warning, but every caller today always has a row.
		if s.buildState != nil {
			buildID := req.GetRequestId()
			_, bsErr := s.buildState.SetReferrer(buildID, referrerDigest, integrityHealth, time.Now().UTC())
			if bsErr != nil && !errors.Is(bsErr, buildstate.ErrNotFound) {
				slog.Warn("buildstate set referrer failed", "build_id", buildID, "err", bsErr)
			}
		}
	}

	// NodeSentinel enqueue — non-fatal; validation is deferred if enqueue fails.
	// Only reached once registration has succeeded (regErr == nil, checked
	// above), so there is always a real CasHash to enqueue against — no
	// validation work is queued for a tool that was never registered.
	if s.sentinel != nil {
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
			CasHash:          regResp.CasHash,
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
	return nil
}

func (s *Service) buildExecution(layerCacheHit bool) *index.BuildExecution {
	if s.builderBackendName() != backendInPodBuildah {
		return nil
	}
	hostUsers := false
	exec := &index.BuildExecution{
		Mode: backendInPodBuildah, HostUsers: &hostUsers,
		StorageDriver: "overlay", Isolation: "chroot",
		LayerCacheHit: &layerCacheHit,
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

// sanitizeTag makes a string safe for use as an image tag component (e.g. a
// version in <tool>:<version>). Unlike sanitizeName, dots are preserved —
// Docker's tag grammar allows them and versions commonly use them
// ("0.7.17"). If the input needed any character substituted or trimmed, a
// short content hash of the original is appended: two different inputs can
// otherwise collapse to the same sanitized string (e.g. "1.0+cuda" and
// "1.0/cuda" both naively become "1.0-cuda"), which would silently conflate
// two different versions under one tag — unacceptable for a reproducibility
// gate. Inputs that are already tag-safe as-is are returned unchanged.
func sanitizeTag(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	clean := strings.Trim(b.String(), "-.")
	if clean == trimmed {
		return truncateTag(clean)
	}
	sum := sha256.Sum256([]byte(trimmed))
	suffix := hex.EncodeToString(sum[:])[:8]
	if clean == "" {
		return suffix
	}
	return truncateTag(clean) + "-" + suffix
}

func truncateTag(s string) string {
	const maxLen = 90 // leaves room for a "-" + 8-char hash suffix under Docker's 128-char tag limit
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// versionedDestination returns the version-pinned tag for a build (see issue
// #27: every build previously pushed only to <tool>:latest, so a tag-based
// pull — as opposed to NodeVault's own digest/casHash-keyed tracking, which
// was never affected — could silently return a different build than
// intended). ok is false when version is empty, since there's nothing
// meaningful to tag with.
func versionedDestination(toolName, version string) (destination string, ok bool) {
	tag := sanitizeTag(version)
	if tag == "" {
		return "", false
	}
	return fmt.Sprintf("%s/library/%s:%s", registryAddr(), sanitizeName(toolName), tag), true
}

func latestDestination(toolName string) string {
	return fmt.Sprintf("%s/library/%s:latest", registryAddr(), sanitizeName(toolName))
}

// primaryBuildDestination returns the tag Build() builds and pushes as this
// build's authoritative reference: the version-pinned tag when a usable
// version is available, otherwise :latest. A failure to push this tag fails
// the build — unlike the :latest alias (see pushLatestAlias), which is a
// convenience pointer only.
func primaryBuildDestination(toolName, version string) (destination string, isVersioned bool) {
	if dest, ok := versionedDestination(toolName, version); ok {
		return dest, true
	}
	return latestDestination(toolName), false
}

// warnIfTagReassigned logs (does not fail the build — see #27's decision to
// allow rebuild-of-the-same-version for now, with a warning rather than a
// hard reject) when destination previously pointed at a different digest.
// ToolImageRecord's (ImageDigest, BuildID) composite key already preserves
// every prior build's own record regardless of tag movement, so no history
// is lost either way — this only adds visibility into the reassignment.
func (s *Service) warnIfTagReassigned(destination, digest string) {
	if s.indexStore == nil {
		return
	}
	prior, err := s.indexStore.GetLatestToolImageRecordByRef(destination)
	if err != nil || prior.ImageDigest == "" || prior.ImageDigest == digest {
		return
	}
	slog.Warn("tag reassigned to a different digest",
		"destination", destination, "previous_digest", prior.ImageDigest, "new_digest", digest)
}

// pushLatestAlias best-effort pushes the :latest convenience tag alongside a
// version-pinned primaryDestination. latest is NOT NodeVault's identity
// source (that's digest/casHash — ToolImageRecord.ImageDigest remains
// authoritative regardless of which tag this build used); it only ever
// reflects whichever build most recently completed this push — not
// necessarily the highest version, and not a validated/certified image.
// Under concurrent builds of different versions, the ordering of two
// pushLatestAlias calls (not build start order, not version order)
// determines the end state. A failure here does not fail the build.
func (s *Service) pushLatestAlias(ctx context.Context, primaryDestination, toolName string, logFn func(string)) {
	dest := latestDestination(toolName)
	if _, err := s.builder.PushTag(ctx, primaryDestination, dest); err != nil {
		slog.Warn("push latest alias failed", "destination", dest, "err", err)
		logFn("latest alias push failed (build itself succeeded): " + err.Error())
		return
	}
	logFn("image also tagged: " + dest)
}
