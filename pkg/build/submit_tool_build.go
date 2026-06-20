package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/index"
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
	if err := stream.Send(buildStateEvent(rec)); err != nil || buildstate.Terminal(rec.Status) {
		return err
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
				continue
			}
			if err := stream.Send(buildStateEvent(rec)); err != nil {
				return err
			}
			lastUpdated = rec.UpdatedAt
			if buildstate.Terminal(rec.Status) {
				return nil
			}
		}
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
		reason = "cancelled by client"
	}
	s.cancelSubmittedBuild(req.GetBuildId())
	rec, err := s.buildState.Transition(
		req.GetBuildId(),
		buildstate.StatusInterrupted,
		fmt.Sprintf("cancelled: %s", reason),
		time.Now().UTC(),
	)
	if err != nil {
		if current, getErr := s.buildState.Get(req.GetBuildId()); getErr == nil && current.Status == buildstate.StatusInterrupted {
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

func buildRequestFromResolved(buildID string, spec index.ResolvedToolSpec) (*nfv1.BuildRequest, error) {
	var req nfv1.BuildRequest
	if err := json.Unmarshal([]byte(spec.RawSpec), &req); err != nil {
		return nil, fmt.Errorf("decode raw_spec JSON: %w", err)
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

func (s *Service) startSubmittedBuild(rec buildstate.Record, req *nfv1.BuildRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	s.activeMu.Lock()
	if s.active == nil {
		s.active = make(map[string]context.CancelFunc)
	}
	s.active[rec.BuildID] = cancel
	s.activeMu.Unlock()
	go s.runSubmittedBuild(ctx, rec, req)
}

func (s *Service) cancelSubmittedBuild(buildID string) {
	s.activeMu.Lock()
	cancel := s.active[buildID]
	s.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) runSubmittedBuild(ctx context.Context, rec buildstate.Record, req *nfv1.BuildRequest) {
	defer func() {
		s.activeMu.Lock()
		delete(s.active, rec.BuildID)
		s.activeMu.Unlock()
	}()
	if _, err := s.buildState.Transition(rec.BuildID, buildstate.StatusResolving, "", time.Now().UTC()); err != nil {
		return
	}
	if _, err := s.buildState.Transition(rec.BuildID, buildstate.StatusBuilding, "", time.Now().UTC()); err != nil {
		return
	}
	if s.builder == nil {
		s.failSubmittedBuild(rec, errors.New("build backend unavailable"))
		return
	}
	destination := fmt.Sprintf("%s/library/%s:latest", registryAddr(), sanitizeName(req.GetToolName()))
	_, digest, nanVersion, err := s.builder.Build(ctx, req.GetDockerfileContent(), destination)
	if err != nil {
		s.failSubmittedBuild(rec, err)
		return
	}
	if _, err := s.buildState.Transition(rec.BuildID, buildstate.StatusPushing, "", time.Now().UTC()); err != nil {
		return
	}
	s.recordBuildSuccess(rec.BuildID, rec.RequestedAt, digest, destination, nanVersion)
	_, _ = s.buildState.Transition(rec.BuildID, buildstate.StatusSucceeded, "", time.Now().UTC())
}

func (s *Service) failSubmittedBuild(rec buildstate.Record, buildErr error) {
	if errors.Is(buildErr, context.Canceled) {
		_, _ = s.buildState.Transition(rec.BuildID, buildstate.StatusInterrupted, "cancelled", time.Now().UTC())
		return
	}
	s.recordBuildFailure(rec.BuildID, rec.RequestedAt, buildErr)
	_, _ = s.buildState.Transition(rec.BuildID, buildstate.StatusFailed, buildErr.Error(), time.Now().UTC())
}

func buildStateEvent(rec buildstate.Record) *nfv1.BuildEvent {
	return &nfv1.BuildEvent{
		Kind:      nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG,
		Message:   fmt.Sprintf("build state: %s", rec.Status),
		Timestamp: rec.UpdatedAt.UnixMilli(),
		BuildId:   rec.BuildID,
		Status:    string(rec.Status),
	}
}
