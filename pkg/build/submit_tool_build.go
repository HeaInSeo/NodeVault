package build

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

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
	if _, err := s.indexStore.GetResolvedToolSpecByDigest(req.GetToolSpecDigest()); err != nil {
		return nil, status.Errorf(codes.NotFound, "resolved tool spec not found: %v", err)
	}

	now := time.Now().UTC()
	rec, err := s.buildState.Create(req.GetRequestId(), req.GetToolSpecDigest(), now)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create build state: %v", err)
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
	return stream.Send(buildStateEvent(rec))
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
	rec, err := s.buildState.Transition(
		req.GetBuildId(),
		buildstate.StatusInterrupted,
		fmt.Sprintf("cancelled: %s", reason),
		time.Now().UTC(),
	)
	if err != nil {
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

func buildStateEvent(rec buildstate.Record) *nfv1.BuildEvent {
	return &nfv1.BuildEvent{
		Kind:      nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG,
		Message:   fmt.Sprintf("build state: %s", rec.Status),
		Timestamp: rec.UpdatedAt.UnixMilli(),
		BuildId:   rec.BuildID,
		Status:    string(rec.Status),
	}
}
