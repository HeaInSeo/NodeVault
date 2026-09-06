package build

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/resolve"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// functionScriptPath is where the frozen v1 script source is baked into the
// function image. W3 only produces a deterministic image that CONTAINS the exact
// script; it deliberately sets no ENTRYPOINT/runtime-execution semantics — how the
// script is invoked at run time is a separate nan/N-track concern, not W3.
const functionScriptPath = "/nodevault/function/source"

// functionBuildRequestFromResolved assembles the internal build request for a W3
// second-image ToolFunction build (BUILD_KIND_TOOLFUNCTIONSPEC) from a resolved
// record whose frozen provenance is nodevault.build.raw_spec.v1.
//
// It fails closed BEFORE any build state is created if the base image cannot be
// resolved to an EXACT NodeVault-built locator: the caller-provided
// base_image_digest must key a persisted ToolImageRecord (the authoritative
// locator — NOT Catalog GetByImageDigest first-match) whose digest matches and
// which carries a non-empty image locator. The generated Dockerfile pulls the base
// strictly by digest, so tag movement cannot alter the base bytes.
//
//nolint:gocritic // hugeParam: by-value snapshot is intentional — matches buildRequestFromResolved.
func (s *Service) functionBuildRequestFromResolved(
	buildID string, spec index.ResolvedToolSpec,
) (*nfv1.BuildRequest, error) {
	if s.indexStore == nil {
		return nil, errors.New("index store unavailable")
	}
	v1, err := resolve.ParseRawSpecV1(spec.RawSpec)
	if err != nil {
		return nil, fmt.Errorf("parse v1 raw_spec: %w", err)
	}
	baseRec, err := s.indexStore.GetToolImageRecordByDigest(v1.BaseImageDigest)
	if err != nil {
		return nil, fmt.Errorf(
			"base image %s does not resolve to a registered NodeVault tool image: %w", v1.BaseImageDigest, err)
	}
	// Defense in depth: the lookup keys on ImageDigest, but re-assert the invariant
	// so a future store change cannot silently return a mismatched record.
	if baseRec.ImageDigest != v1.BaseImageDigest {
		return nil, fmt.Errorf(
			"base image record digest mismatch: record=%q requested=%q", baseRec.ImageDigest, v1.BaseImageDigest)
	}
	if strings.TrimSpace(baseRec.ImageRef) == "" {
		return nil, fmt.Errorf("base image record for %s has no image locator", v1.BaseImageDigest)
	}
	dockerfile, err := renderFunctionDockerfile(baseRec.ImageRef, v1.BaseImageDigest, v1.Script)
	if err != nil {
		return nil, err
	}
	toolName := spec.ToolName
	if strings.TrimSpace(toolName) == "" {
		return nil, errors.New("resolved tool spec has no tool_name")
	}
	return &nfv1.BuildRequest{
		RequestId:         buildID,
		Kind:              nfv1.BuildKind_BUILD_KIND_TOOLFUNCTIONSPEC,
		ToolName:          toolName,
		Version:           spec.Version,
		BaseImageDigest:   v1.BaseImageDigest,
		DockerfileContent: dockerfile,
	}, nil
}

// renderFunctionDockerfile deterministically renders the internal Dockerfile for a
// function-image build: FROM the base pinned by EXACT digest (tag-immune), then the
// script source baked in via a heredoc COPY. The heredoc terminator is derived from
// the script's own hash so it cannot collide with the script content.
//
// The rendering is a pure function of (baseRef, baseDigest, script): an identical
// function spec yields an identical build recipe and any script change changes it.
// Buildah's heredoc COPY normalizes the BAKED file's line endings (strips CR, adds
// a trailing newline), so the baked /nodevault/function/source is not guaranteed
// byte-identical to the source; a source-sha256 label over the exact bytes makes
// any byte-level difference (CRLF vs LF, trailing newline) still produce a distinct
// image, so distinct declared scripts never collide onto one function image. Exact
// baked-byte fidelity matters only for script execution, which W3 does not perform
// (nan/N-track).
func renderFunctionDockerfile(baseRef, baseDigest, script string) (string, error) {
	base := baseImageByDigest(baseRef, baseDigest)
	sum := sha256.Sum256([]byte(script))
	marker := "NODEVAULT_FN_EOF_" + hex.EncodeToString(sum[:])[:32]
	for _, line := range strings.Split(script, "\n") {
		if line == marker {
			// Astronomically impossible for a hash-derived marker, but never emit a
			// heredoc whose terminator appears inside its own body.
			return "", errors.New("function script collides with the generated heredoc terminator")
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", base)
	// Bind the image to the EXACT script bytes via a label over their sha256. The
	// heredoc COPY below normalizes the baked file's line endings (Buildah's parser
	// strips CR and adds a trailing newline), so two byte-different scripts (e.g.
	// CRLF vs LF, or with/without a trailing newline) could otherwise build the same
	// image; this label makes any byte-level difference produce a distinct image
	// config and therefore a distinct function_image_digest, so distinct declared
	// scripts never collide onto one function image. (Script execution semantics,
	// where the baked bytes would matter, are out of W3 scope — nan/N-track.)
	fmt.Fprintf(&b, "LABEL io.nodevault.function.source-sha256=%q\n", hex.EncodeToString(sum[:]))
	fmt.Fprintf(&b, "COPY <<'%s' %s\n", marker, functionScriptPath)
	b.WriteString(script)
	if !strings.HasSuffix(script, "\n") {
		// Terminate the last script line and separate it from the heredoc marker.
		// This normalizes the baked file to a single trailing newline (see the
		// function doc) — Buildah's heredoc writes one regardless.
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "%s\n", marker)
	return b.String(), nil
}

// baseImageByDigest returns a digest-pinned reference (repo@sha256:...) from a
// stored image locator (which may carry a :tag) and the exact digest, dropping any
// tag/existing digest so the FROM cannot be steered by tag movement. A registry
// host :port is preserved (only a tag after the last '/' is stripped).
func baseImageByDigest(imageRef, digest string) string {
	repo := imageRef
	if at := strings.LastIndex(repo, "@"); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
		repo = repo[:colon]
	}
	return repo + "@" + digest
}

// functionDestination is the deterministic push target for a function image. It is
// namespaced with a distinct :toolfn-<digest> tag so a function image can never
// overwrite the base tool's :latest/:version tags; the authoritative identity is
// the recorded function_image_digest, not this locator tag.
func functionDestination(toolName, toolSpecDigest string) string {
	return fmt.Sprintf("%s/library/%s:toolfn-%s",
		registryAddr(), sanitizeName(toolName), sanitizeTag(shortDigest(toolSpecDigest)))
}

// shortDigest strips a sha256: prefix and returns the first 16 hex chars for use
// in a locator tag.
func shortDigest(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 16 {
		d = d[:16]
	}
	return d
}
