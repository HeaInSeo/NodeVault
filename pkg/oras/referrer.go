// Package oras wraps sori's experimental referrer push helpers for NodeVault.
// It provides a thin, NodeVault-specific interface over the sori OCI referrer API.
//
// Caller contract:
//   - PushToolSpecReferrer: called by pkg/build after L4 + registration succeed.
//   - If the push fails, the caller logs a warning and leaves integrity_health=Partial;
//     the reconcile loop will retry on the next cycle.
package oras

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/seoyhaein/sori"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// toolSpec is the JSON payload written as an OCI referrer artifact.
// mediaType: application/vnd.nodevault.toolspec.v1+json
type toolSpec struct {
	ToolName  string `json:"tool_name"`
	Version   string `json:"version,omitempty"`
	StableRef string `json:"stable_ref,omitempty"`
	ImageURI  string `json:"image_uri,omitempty"`
	Digest    string `json:"digest,omitempty"`
	CasHash   string `json:"cas_hash,omitempty"`
}

// PushToolSpecReferrer attaches a tool spec as an OCI referrer to the image
// identified by imageRepo and subjectDigest.
//
// imageRepo is the Harbor repository reference without tag or digest,
// e.g. "harbor.10.113.24.96.nip.io/library/mytool".
// subjectDigest is the image manifest digest, e.g. "sha256:abc...".
//
// Returns the referrer manifest digest on success.
// A non-nil error means the push failed; the caller should log and continue —
// integrity_health will remain Partial until the reconcile loop retries.
func PushToolSpecReferrer(
	ctx context.Context, imageRepo, subjectDigest string, tool *nfv1.RegisteredToolDefinition,
) (string, error) {
	if imageRepo == "" {
		return "", fmt.Errorf("oras: imageRepo must not be empty")
	}
	if subjectDigest == "" {
		return "", fmt.Errorf("oras: subjectDigest must not be empty")
	}
	if tool == nil {
		return "", fmt.Errorf("oras: tool must not be nil")
	}

	target, err := sori.NewReferrerRemoteRepository(imageRepo, true, nil)
	if err != nil {
		return "", fmt.Errorf("oras: create remote repository %q: %w", imageRepo, err)
	}

	spec := toolSpec{
		ToolName:  tool.ToolName,
		Version:   tool.Version,
		StableRef: tool.StableRef,
		ImageURI:  tool.ImageUri,
		Digest:    tool.Digest,
		CasHash:   tool.CasHash,
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("oras: marshal tool spec: %w", err)
	}

	result, err := sori.PushToolSpecReferrer(ctx, target, subjectDigest, specJSON)
	if err != nil {
		return "", fmt.Errorf("oras: push referrer to %q: %w", imageRepo, err)
	}
	return result.ReferrerDigest, nil
}
