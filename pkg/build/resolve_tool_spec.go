package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ResolveToolSpec implements BuildServiceServer.
// It content-addresses the incoming raw_spec (sha256 hex of the raw bytes, no
// re-marshaling) and records a ResolvedToolSpec in the index. Repeated requests
// carrying byte-identical raw_spec are idempotent: the existing record is
// returned instead of erroring.
func (s *Service) ResolveToolSpec(
	_ context.Context, req *nfv1.ToolSpecRequest,
) (*nfv1.ResolvedToolSpecResponse, error) {
	if req.GetRawSpec() == "" || req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_name and raw_spec are required")
	}
	if s.indexStore == nil {
		return nil, status.Error(codes.Unavailable, "build backend disabled: index store unavailable")
	}

	sum := sha256.Sum256([]byte(req.GetRawSpec()))
	digest := hex.EncodeToString(sum[:])

	rec := index.ResolvedToolSpec{
		ToolSpecDigest: digest,
		ToolName:       req.GetToolName(),
		Version:        req.GetVersion(),
		RawSpec:        req.GetRawSpec(),
		ResolvedAt:     time.Now().UTC(),
	}

	if err := s.indexStore.AppendResolvedToolSpec(rec); err != nil {
		// pkg/index/store.go's AppendResolvedToolSpec does not export a sentinel
		// for the duplicate-digest case; it returns a plain (unwrapped) error built
		// via fmt.Errorf("index: resolved tool spec %q already exists", ...). Match
		// on the exact message it produces for this digest.
		dupErr := fmt.Sprintf("index: resolved tool spec %q already exists", digest)
		if err.Error() == dupErr {
			existing, getErr := s.indexStore.GetResolvedToolSpecByDigest(digest)
			if getErr != nil {
				return nil, status.Errorf(codes.Internal, "resolved tool spec lookup: %v", getErr)
			}
			return &nfv1.ResolvedToolSpecResponse{
				ToolSpecDigest: existing.ToolSpecDigest,
				ToolName:       existing.ToolName,
				Version:        existing.Version,
				ResolvedAt:     existing.ResolvedAt.UnixMilli(),
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "resolved tool spec append: %v", err)
	}

	return &nfv1.ResolvedToolSpecResponse{
		ToolSpecDigest: rec.ToolSpecDigest,
		ToolName:       rec.ToolName,
		Version:        rec.Version,
		ResolvedAt:     rec.ResolvedAt.UnixMilli(),
	}, nil
}
