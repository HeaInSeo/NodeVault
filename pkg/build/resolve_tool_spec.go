package build

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ResolveToolSpec implements BuildServiceServer.
// It content-addresses the incoming ToolSpecRequest through pkg/resolve and
// records a ResolvedToolSpec in the index. Repeated requests producing the same
// toolSpecDigest are idempotent: the existing record is returned instead of
// erroring.
func (s *Service) ResolveToolSpec(
	_ context.Context, req *nfv1.ToolSpecRequest,
) (*nfv1.ResolvedToolSpecResponse, error) {
	if req.GetRawSpec() == "" || req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_name and raw_spec are required")
	}
	if s.indexStore == nil {
		return nil, status.Error(codes.Unavailable, "build backend disabled: index store unavailable")
	}

	resolved, err := resolve.Resolve(resolve.Request{
		ToolName: req.GetToolName(),
		Version:  req.GetVersion(),
		RawSpec:  req.GetRawSpec(),
	}, resolve.Context{})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "resolve tool spec digest: %v", err)
	}

	rec := index.ResolvedToolSpec{
		ToolSpecDigest:     resolved.ToolSpecDigest,
		ToolName:           req.GetToolName(),
		Version:            req.GetVersion(),
		RawSpec:            req.GetRawSpec(),
		RecipeInputsDigest: resolved.RecipeInputsDigest,
		BuildPlanDigest:    resolved.BuildPlanDigest,
		BuilderIdentity:    resolved.BuilderIdentity,
		BaseImageRef:       resolved.BaseImageRef,
		BaseImageDigest:    resolved.BaseImageDigest,
		ResolvedAt:         time.Now().UTC(),
	}

	stored, err := s.indexStore.UpsertResolvedToolSpec(rec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolved tool spec append: %v", err)
	}

	return &nfv1.ResolvedToolSpecResponse{
		ToolSpecDigest: stored.ToolSpecDigest,
		ToolName:       stored.ToolName,
		Version:        stored.Version,
		ResolvedAt:     stored.ResolvedAt.UnixMilli(),
	}, nil
}
