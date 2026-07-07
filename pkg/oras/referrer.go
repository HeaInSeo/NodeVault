// Package oras wraps sori's experimental referrer push helpers for NodeVault.
// It provides a thin, NodeVault-specific interface over the sori OCI referrer API.
//
// Caller contract:
//   - PushToolSpecReferrer: called by pkg/build after L4 + registration succeed.
//   - PushToolProfileReferrer: called by pkg/validation after a successful
//     ToolCheckRecord (validation_status=succeeded, validation_hash set) is stored.
//   - If a push fails, the caller logs a warning and continues; the relevant
//     digest field simply stays unset until a future retry succeeds.
package oras

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/HeaInSeo/sori"
	"github.com/HeaInSeo/sori/registryutil"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// toolSpec is the JSON payload written as an OCI referrer artifact.
// mediaType: application/vnd.nodevault.toolspec.v1+json
// This is build-time ToolSpec metadata only. ToolFunctionSpec metadata such as
// command, inputs, outputs, and display is intentionally excluded from this
// referrer and belongs to the function/validation profile path.
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
// e.g. "harbor.lab.local/library/mytool".
// subjectDigest is the image manifest digest, e.g. "sha256:abc...".
//
// TLS behavior is controlled by env vars:
//
//	NODEVAULT_ORAS_INSECURE_TLS=true  — skip TLS verification (self-signed certs)
//	NODEVAULT_ORAS_CA_FILE=/path/cert.pem — use custom CA
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

	target, err := newRemoteRepository(imageRepo)
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

// toolProfile is the JSON payload written as an OCI referrer artifact.
// mediaType: application/vnd.nodevault.toolprofile.v1+json
// Fields mirror docs/OBSERVED_PROFILE_SPEC.md §2.1's "profile" object.
type toolProfile struct {
	CasHash                 string                         `json:"cas_hash,omitempty"`
	ToolSpecDigest          string                         `json:"tool_spec_digest,omitempty"`
	ValidationHash          string                         `json:"validation_hash,omitempty"`
	ValidationStatus        string                         `json:"validation_status,omitempty"`
	Command                 string                         `json:"command,omitempty"`
	ExitCode                int                            `json:"exit_code,omitempty"`
	ObservedIoProfile       *index.ObservedIoProfile       `json:"observed_io_profile,omitempty"`
	ObservedResourceProfile *index.ObservedResourceProfile `json:"observed_resource_profile,omitempty"`
	ContractCheck           *index.ContractCheck           `json:"contract_check,omitempty"`
}

// PushToolProfileReferrer attaches an observed L5-a functional validation
// profile as an OCI referrer to the image identified by imageRepo and
// subjectDigest.
//
// imageRepo and subjectDigest follow the same convention as PushToolSpecReferrer.
// casHash identifies the index.Entry the digest should be recorded against;
// check is the stored validation record that produced the profile.
//
// Returns the referrer manifest digest on success. A non-nil error means the
// push failed; the caller should log and continue — ObservedProfileDigest
// simply stays unset until a future validation run retries the push.
func PushToolProfileReferrer(
	ctx context.Context, imageRepo, subjectDigest, casHash string, check *index.ToolCheckRecord,
) (string, error) {
	if imageRepo == "" {
		return "", fmt.Errorf("oras: imageRepo must not be empty")
	}
	if subjectDigest == "" {
		return "", fmt.Errorf("oras: subjectDigest must not be empty")
	}
	if check == nil {
		return "", fmt.Errorf("oras: check must not be nil")
	}

	target, err := newRemoteRepository(imageRepo)
	if err != nil {
		return "", fmt.Errorf("oras: create remote repository %q: %w", imageRepo, err)
	}

	profile := toolProfile{
		CasHash:                 casHash,
		ToolSpecDigest:          check.ToolSpecDigest,
		ValidationHash:          check.ValidationHash,
		ValidationStatus:        check.ValidationStatus,
		Command:                 check.Command,
		ExitCode:                check.ExitCode,
		ObservedIoProfile:       check.ObservedIoProfile,
		ObservedResourceProfile: check.ObservedResourceProfile,
		ContractCheck:           check.ContractCheck,
	}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("oras: marshal tool profile: %w", err)
	}

	result, err := sori.PushToolProfileReferrer(ctx, target, subjectDigest, profileJSON)
	if err != nil {
		return "", fmt.Errorf("oras: push referrer to %q: %w", imageRepo, err)
	}
	return result.ReferrerDigest, nil
}

// newRemoteRepository builds a remote repository client using env-driven TLS
// config. Credentials are read from the Docker/containers auth.json file
// (REGISTRY_AUTH_FILE env var, default /run/containers/0/auth.json) — the
// same file Buildah uses for pull/push, so no separate Secret is required.
func newRemoteRepository(imageRepo string) (sori.ReferrerTarget, error) {
	registry := registryFromRepo(imageRepo)
	username, password, err := credentialsFromAuthFile(registry)
	if err != nil {
		return nil, fmt.Errorf("oras: load credentials for %q: %w", registry, err)
	}
	cfg := registryutil.RemoteConfig{
		InsecureTLS: os.Getenv("NODEVAULT_ORAS_INSECURE_TLS") == "true",
		CAFile:      os.Getenv("NODEVAULT_ORAS_CA_FILE"),
		Username:    username,
		Password:    password,
	}
	repo, err := registryutil.NewRepository(imageRepo, cfg)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

// registryFromRepo extracts the registry host from an image repository reference.
// "harbor.lab.local/library/mytool" → "harbor.lab.local"
func registryFromRepo(imageRepo string) string {
	if idx := strings.IndexByte(imageRepo, '/'); idx != -1 {
		return imageRepo[:idx]
	}
	return imageRepo
}

// credentialsFromAuthFile reads username and password for registry from the
// Docker/containers auth.json file. The file path is taken from the
// REGISTRY_AUTH_FILE env var, defaulting to /run/containers/0/auth.json.
// Returns ("", "", nil) when the file is absent or has no entry for registry.
func credentialsFromAuthFile(registry string) (username, password string, err error) {
	path := os.Getenv("REGISTRY_AUTH_FILE")
	if path == "" {
		path = "/run/containers/0/auth.json"
	}
	//nolint:gosec // path is operator-controlled via REGISTRY_AUTH_FILE or hardcoded default
	data, readErr := os.ReadFile(path)
	if os.IsNotExist(readErr) {
		return "", "", nil
	}
	if readErr != nil {
		return "", "", fmt.Errorf("read auth file %q: %w", path, readErr)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if parseErr := json.Unmarshal(data, &cfg); parseErr != nil {
		return "", "", fmt.Errorf("parse auth file: %w", parseErr)
	}
	entry, ok := cfg.Auths[registry]
	if !ok {
		return "", "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", "", fmt.Errorf("decode auth for %q: %w", registry, err)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("auth entry for %q is not user:password format", registry)
	}
	return parts[0], parts[1], nil
}
