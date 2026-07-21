package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const watchPollInterval = 100 * time.Millisecond

// SubmitToolBuild records an asynchronous build request for a resolved tool spec.
func (s *Service) SubmitToolBuild(
	_ context.Context, req *nfv1.SubmitToolBuildRequest,
) (*nfv1.SubmitToolBuildResponse, error) {
	if req.GetRequestId() == "" || req.GetToolSpecDigest() == "" {
		return nil, status.Error(codes.InvalidArgument, "request_id and tool_spec_digest are required")
	}
	if s.indexStore == nil || s.buildState == nil {
		return nil, status.Error(codes.Unavailable, "build state unavailable")
	}
	spec, err := s.indexStore.GetResolvedToolSpecByDigest(req.GetToolSpecDigest())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "resolved tool spec not found: %v", err)
	}
	buildReq, err := buildRequestFromResolved(req.GetRequestId(), spec)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "resolved tool spec is not buildable: %v", err)
	}
	if err = ValidateBuildRequest(buildReq); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "resolved tool spec failed build policy: %v", err)
	}

	now := time.Now().UTC()
	rec, created, err := s.buildState.CreateOrGet(req.GetRequestId(), req.GetToolSpecDigest(), now)
	if err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "create build state: %v", err)
	}
	if created {
		s.startSubmittedBuild(rec, buildReq)
	}
	return &nfv1.SubmitToolBuildResponse{
		BuildId:        rec.BuildID,
		ToolSpecDigest: rec.ToolSpecDigest,
		Status:         string(rec.Status),
		SubmittedAt:    rec.RequestedAt.UnixMilli(),
	}, nil
}

// WatchToolBuild emits the current durable state snapshot for a submitted build.
func (s *Service) WatchToolBuild(
	req *nfv1.WatchToolBuildRequest,
	stream grpc.ServerStreamingServer[nfv1.BuildEvent],
) error {
	if req.GetBuildId() == "" {
		return status.Error(codes.InvalidArgument, "build_id is required")
	}
	if s.buildState == nil {
		return status.Error(codes.Unavailable, "build state unavailable")
	}
	rec, err := s.buildState.Get(req.GetBuildId())
	if err != nil {
		if errors.Is(err, buildstate.ErrNotFound) {
			return status.Errorf(codes.NotFound, "build not found: %s", req.GetBuildId())
		}
		return status.Errorf(codes.Internal, "get build state: %v", err)
	}
	if sendErr := stream.Send(buildStateEvent(rec)); sendErr != nil || buildstate.Terminal(rec.Status) {
		return sendErr
	}

	ticker := time.NewTicker(watchPollInterval)
	defer ticker.Stop()
	lastUpdated := rec.UpdatedAt
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
			rec, err = s.buildState.Get(req.GetBuildId())
			if err != nil {
				return status.Errorf(codes.Internal, "get build state: %v", err)
			}
			if rec.UpdatedAt.Equal(lastUpdated) {
				// Durable state hasn't moved. This is the normal case for a
				// build that's still genuinely running, but it's also what
				// a build stuck forever because its own buildstate write
				// failed looks like from here — the write that would have
				// made progress visible is exactly the one that failed. The
				// active-build registry is the only place that failure is
				// recorded, so check it before deciding to keep waiting.
				if failErr := s.activeBuildFailure(req.GetBuildId()); failErr != nil {
					return status.Errorf(codes.Internal,
						"build %s abandoned: buildstate could not be updated: %v", req.GetBuildId(), failErr)
				}
				continue
			}
			if sendErr := stream.Send(buildStateEvent(rec)); sendErr != nil {
				return sendErr
			}
			lastUpdated = rec.UpdatedAt
			if buildstate.Terminal(rec.Status) {
				return nil
			}
		}
	}
}

// activeBuildFailure reports the failure recorded for buildID's in-flight
// goroutine, if any is currently tracked. Returns nil when the build is
// still running normally, when it already finished through the normal
// durable-terminal path (its entry is removed then), or when it hasn't
// failed.
func (s *Service) activeBuildFailure(buildID string) error {
	s.activeMu.Lock()
	entry := s.active[buildID]
	s.activeMu.Unlock()
	if entry == nil {
		return nil
	}
	select {
	case <-entry.done:
		return entry.err
	default:
		return nil
	}
}

// CancelToolBuild marks a non-terminal submitted build as Interrupted.
func (s *Service) CancelToolBuild(
	_ context.Context, req *nfv1.CancelToolBuildRequest,
) (*nfv1.CancelToolBuildResponse, error) {
	if req.GetBuildId() == "" {
		return nil, status.Error(codes.InvalidArgument, "build_id is required")
	}
	if s.buildState == nil {
		return nil, status.Error(codes.Unavailable, "build state unavailable")
	}
	reason := req.GetReason()
	if reason == "" {
		reason = "canceled by client"
	}
	s.cancelSubmittedBuild(req.GetBuildId())
	rec, err := s.buildState.Transition(
		req.GetBuildId(),
		buildstate.StatusInterrupted,
		fmt.Sprintf("canceled: %s", reason),
		time.Now().UTC(),
	)
	if err != nil {
		if current, getErr := s.buildState.Get(req.GetBuildId()); getErr == nil &&
			current.Status == buildstate.StatusInterrupted {
			return &nfv1.CancelToolBuildResponse{
				BuildId:     current.BuildID,
				Status:      string(current.Status),
				CancelledAt: current.UpdatedAt.UnixMilli(),
			}, nil
		}
		if errors.Is(err, buildstate.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "build not found: %s", req.GetBuildId())
		}
		return nil, status.Errorf(codes.FailedPrecondition, "cancel build: %v", err)
	}
	return &nfv1.CancelToolBuildResponse{
		BuildId:     rec.BuildID,
		Status:      string(rec.Status),
		CancelledAt: rec.UpdatedAt.UnixMilli(),
	}, nil
}

//nolint:gocritic // hugeParam: by-value snapshot is intentional — read-only helper, no pointer lifetime risk.
func buildRequestFromResolved(buildID string, spec index.ResolvedToolSpec) (*nfv1.BuildRequest, error) {
	var req nfv1.BuildRequest
	dec := json.NewDecoder(bytes.NewReader([]byte(spec.RawSpec)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("decode raw_spec JSON: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode raw_spec JSON: unexpected trailing content after the first JSON value")
	}
	if req.GetDockerfileContent() == "" {
		return nil, errors.New("dockerfile_content is required")
	}
	if req.GetToolName() == "" {
		req.ToolName = spec.ToolName
	}
	if req.GetVersion() == "" {
		req.Version = spec.Version
	}
	if req.GetToolName() == "" {
		return nil, errors.New("tool_name is required")
	}
	req.RequestId = buildID
	return &req, nil
}

//nolint:gocritic // hugeParam: by-value snapshot is intentional — goroutine-safe, no shared mutation.
func (s *Service) startSubmittedBuild(rec buildstate.Record, req *nfv1.BuildRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	entry := &activeBuild{cancel: cancel, done: make(chan struct{})}
	s.activeMu.Lock()
	if s.active == nil {
		s.active = make(map[string]*activeBuild)
	}
	s.active[rec.BuildID] = entry
	s.activeMu.Unlock()
	go s.runSubmittedBuild(ctx, cancel, rec, req)
}

func (s *Service) cancelSubmittedBuild(buildID string) {
	s.activeMu.Lock()
	entry := s.active[buildID]
	s.activeMu.Unlock()
	if entry != nil {
		entry.cancel()
	}
}

//nolint:gocritic // hugeParam: by-value snapshot is intentional — goroutine-safe, no shared mutation.
func (s *Service) runSubmittedBuild(
	ctx context.Context, cancel context.CancelFunc, rec buildstate.Record, req *nfv1.BuildRequest,
) {
	defer cancel()
	if _, err := s.buildState.Transition(rec.BuildID, buildstate.StatusResolving, "", time.Now().UTC()); err != nil {
		s.abandonSubmittedBuild(rec, "resolving", err)
		return
	}
	if _, err := s.buildState.Transition(rec.BuildID, buildstate.StatusBuilding, "", time.Now().UTC()); err != nil {
		s.abandonSubmittedBuild(rec, "building", err)
		return
	}
	if s.builder == nil {
		s.failSubmittedBuild(rec, errors.New("build backend unavailable"))
		return
	}
	destination, isVersioned := primaryBuildDestination(req.GetToolName(), req.GetVersion())
	_, digest, layerCacheHit, err := s.builder.Build(ctx, req.GetDockerfileContent(), destination)
	if err != nil {
		s.failSubmittedBuild(rec, err)
		return
	}
	s.warnIfTagReassigned(destination, digest)
	if _, err := s.buildState.Transition(rec.BuildID, buildstate.StatusPushing, "", time.Now().UTC()); err != nil {
		s.abandonSubmittedBuild(rec, "pushing", err)
		return
	}
	if _, err := s.buildState.SetArtifact(rec.BuildID, destination, digest, time.Now().UTC()); err != nil {
		slog.Warn("buildstate set artifact failed", "build_id", rec.BuildID, "err", err)
	}
	s.recordBuildSuccess(rec.BuildID, rec.RequestedAt, digest, destination, layerCacheHit)

	logFn := func(msg string) { slog.Info("submitted build", "build_id", rec.BuildID, "msg", msg) }
	if isVersioned {
		s.pushLatestAlias(ctx, destination, req.GetToolName(), logFn)
	}
	if regErr := s.postBuildRegistration(ctx, req, destination, digest, logFn); regErr != nil {
		// Image build+push already succeeded — SetArtifact above already
		// persisted ImageRef/ImageDigest on this buildstate record, so they
		// remain visible on the FAILED terminal event too (buildStateEvent
		// always includes them regardless of status). Only cataloging
		// failed: reported as Failed, not Succeeded, since an unregistered
		// tool is not discoverable/usable (see #23). Retrying resubmits the
		// same destination/digest; the image itself is not rebuilt.
		slog.Error("submitted build registration failed", "build_id", rec.BuildID, "err", regErr)
		msg := fmt.Sprintf("image pushed to %s@%s but registration failed: %v", destination, digest, regErr)
		s.finalizeSubmittedBuild(rec, "registering", buildstate.StatusFailed, msg)
		return
	}

	s.finalizeSubmittedBuild(rec, "succeeding", buildstate.StatusSucceeded, "")
}

//nolint:gocritic // hugeParam: by-value snapshot is intentional — read-only helper, no pointer needed.
func (s *Service) failSubmittedBuild(rec buildstate.Record, buildErr error) {
	if errors.Is(buildErr, context.Canceled) {
		s.finalizeSubmittedBuild(rec, "interrupting", buildstate.StatusInterrupted, "canceled")
		return
	}
	s.recordBuildFailure(rec.BuildID, rec.RequestedAt, buildErr)
	s.finalizeSubmittedBuild(rec, "failing", buildstate.StatusFailed, buildErr.Error())
}

// finalizeSubmittedBuild attempts the terminal buildstate transition for a
// completed build. If the write succeeds, buildstate is the authoritative
// durable record and the active-build entry is removed — any WatchToolBuild
// caller sees the terminal state on its next poll through the normal
// durable-read path. If the write itself fails, that's handled exactly like
// any other mid-build buildstate write failure (see abandonSubmittedBuild):
// the entry is kept and its failure broadcast instead of the error being
// silently discarded.
//
// A Transition failure reporting the build already terminal is not an
// abandonment — it means another caller (CancelToolBuild, most likely)
// already wrote a terminal status for this build first. That's a normal
// race, not a storage failure, and the durable record is already reachable
// through the ordinary poll loop.
//
//nolint:gocritic // hugeParam: by-value snapshot is intentional — read-only helper, no pointer needed.
func (s *Service) finalizeSubmittedBuild(
	rec buildstate.Record, atStage string, next buildstate.Status, message string,
) {
	if _, err := s.buildState.Transition(rec.BuildID, next, message, time.Now().UTC()); err != nil {
		if errors.Is(err, buildstate.ErrAlreadyTerminal) {
			s.activeMu.Lock()
			delete(s.active, rec.BuildID)
			s.activeMu.Unlock()
			return
		}
		s.abandonSubmittedBuild(rec, atStage, err)
		return
	}
	recordBuildOutcomeMetric(next)
	s.activeMu.Lock()
	delete(s.active, rec.BuildID)
	s.activeMu.Unlock()
}

// recordBuildOutcomeMetric increments the operational build-outcome counter
// for a durably-confirmed terminal transition — called exactly once per
// build, only after buildState.Transition itself has succeeded (a write
// that failed isn't a confirmed outcome; see abandonSubmittedBuild). Only
// Succeeded/Failed are tracked, matching what the now-removed legacy
// BuildAndRegister RPC used to record (issue #15) — Interrupted (user
// cancel, or a process-restart recovery sweep) is deliberately excluded so
// operators can distinguish real build failures from cancellation.
func recordBuildOutcomeMetric(terminalStatus buildstate.Status) {
	switch terminalStatus {
	case buildstate.StatusSucceeded:
		metrics.BuildSuccessTotal.Add(1)
	case buildstate.StatusFailed:
		metrics.BuildFailureTotal.Add(1)
	}
}

// abandonSubmittedBuild handles the case where buildState.Transition itself
// failed (e.g. a local SQLite write error) — the build cannot be moved to a
// terminal state through the same store that just failed to write, so a
// WatchToolBuild caller would otherwise hang with no further events forever.
// This makes the failure loud (slog.Error, unlike the best-effort slog.Warn
// used elsewhere for non-critical writes), records it in pkg/index — a
// separate store from buildstate — so there is at least one durable,
// queryable record of the failure, and broadcasts it through the
// active-build registry so any WatchToolBuild caller, present or future,
// learns the build was abandoned instead of polling a buildstate record
// stuck at whatever status it last reached forever.
//
// Deliberately does not remove the build's entry from s.active: a
// WatchToolBuild caller that connects (or next polls) after this point must
// still be able to find the entry and observe the failure. The entry
// persists until process restart (see issue #26 follow-up for TTL/cap on
// abandoned entries — not needed today since NodeVault is single-replica
// and abandonment is rare).
//
//nolint:gocritic // hugeParam: by-value snapshot is intentional — read-only helper, no pointer needed.
func (s *Service) abandonSubmittedBuild(rec buildstate.Record, atStage string, transitionErr error) {
	slog.Error("buildstate transition failed; build abandoned at this stage and will not reach a terminal state",
		"build_id", rec.BuildID, "stage", atStage, "err", transitionErr)
	wrapped := fmt.Errorf("buildstate transition to %s failed: %w", atStage, transitionErr)
	s.recordBuildFailure(rec.BuildID, rec.RequestedAt, wrapped)

	s.activeMu.Lock()
	entry := s.active[rec.BuildID]
	s.activeMu.Unlock()
	if entry != nil {
		entry.fail(wrapped)
	}
}

//nolint:gocritic // hugeParam: by-value snapshot is intentional — read-only helper, no pointer lifetime risk.
func buildStateEvent(rec buildstate.Record) *nfv1.BuildEvent {
	return &nfv1.BuildEvent{
		Kind:               nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG,
		Message:            fmt.Sprintf("build state: %s", rec.Status),
		Timestamp:          rec.UpdatedAt.UnixMilli(),
		BuildId:            rec.BuildID,
		Status:             string(rec.Status),
		ImageRef:           rec.ImageRef,
		ImageDigest:        rec.ImageDigest,
		SpecReferrerDigest: rec.SpecReferrerDigest,
		IntegrityHealth:    rec.IntegrityHealth,
	}
}
