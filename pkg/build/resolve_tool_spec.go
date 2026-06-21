package build

import (
	"context"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/registry"
	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// baseImageResolver resolves an unpinned (tag-only) base image ref to its
// manifest digest. Implemented by *registry.Client in production; tests may
// substitute a fake.
type baseImageResolver interface {
	ResolveTagDigest(ctx context.Context, ref string) (string, error)
}

// resolveUnpinnedBaseImageEnabled reports whether ResolveToolSpec should
// auto-resolve an unpinned base image ref to a digest via a registry lookup,
// instead of rejecting the request. Off by default: operators who require
// Harbor-only strictness (no live registry dependency in the resolve path)
// keep today's reject-unpinned behavior unchanged. This is an operator
// policy choice, not an architectural one - some deployments will want it
// on to support tag-only authoring against public base images, others will
// not.
func resolveUnpinnedBaseImageEnabled() bool {
	return os.Getenv("NODEVAULT_RESOLVE_UNPINNED_BASE_IMAGE") == "true"
}

// ResolveToolSpec implements BuildServiceServer.
// It content-addresses the incoming ToolSpecRequest through pkg/resolve and
// records a ResolvedToolSpec in the index. Repeated requests producing the same
// toolSpecDigest are idempotent: the existing record is returned instead of
// erroring.
func (s *Service) ResolveToolSpec(
	ctx context.Context, req *nfv1.ToolSpecRequest,
) (*nfv1.ResolvedToolSpecResponse, error) {
	if req.GetRawSpec() == "" || req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_name and raw_spec are required")
	}
	if s.indexStore == nil {
		return nil, status.Error(codes.Unavailable, "build backend disabled: index store unavailable")
	}

	resolveCtx := resolve.Context{}
	if resolveUnpinnedBaseImageEnabled() {
		baseImageRef, baseImageDigest := resolve.BaseImagePin(req.GetRawSpec())
		if baseImageRef != "" && baseImageDigest == "" {
			resolver := s.baseImageResolver
			if resolver == nil {
				resolver = registry.NewClient()
			}
			digest, err := resolver.ResolveTagDigest(ctx, baseImageRef)
			if err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "resolve unpinned base image digest: %v", err)
			}
			resolveCtx.BaseImageDigest = digest
		}
	}

	resolved, err := resolve.Resolve(resolve.Request{
		ToolName: req.GetToolName(),
		Version:  req.GetVersion(),
		RawSpec:  req.GetRawSpec(),
	}, resolveCtx)
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
