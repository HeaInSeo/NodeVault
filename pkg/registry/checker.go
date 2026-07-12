package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/HeaInSeo/NodeVault/pkg/registryconfig"
)

// HarborChecker implements reconcile.RegistryChecker using the OCI Distribution Spec API.
//
// imageRef must be in the form "host/project/repo:tag" (stored in index.Entry.ImageRef).
// digest must be the manifest digest "sha256:...".
//
// Scheme and TLS trust come from the registryconfig.Config passed to
// NewHarborChecker — the same settings pkg/oras uses for referrer push, so
// reconcile no longer trusts a different CA or scheme than the rest of
// NodeVault.
//
// Outcome contract: a 200 response means (true, nil). A confirmed 404 means
// (false, nil) — not found. Anything else (401/403/5xx/timeout/TLS failure)
// is indeterminate and returns (false, err); callers must not treat that
// error as "not found," since doing so would misclassify an auth challenge
// or transient failure as a missing artifact.
type HarborChecker struct {
	client *Client
	scheme string
}

// NewHarborChecker creates a HarborChecker using cfg's scheme and CA trust.
func NewHarborChecker(cfg registryconfig.Config) (*HarborChecker, error) {
	httpClient, err := cfg.HTTPClient()
	if err != nil {
		return nil, fmt.Errorf("registry: build HTTP client: %w", err)
	}
	return &HarborChecker{client: newClientWithHTTP(httpClient), scheme: cfg.Scheme}, nil
}

// ImageExists checks whether the manifest identified by digest exists in the registry.
// Uses HEAD /v2/{name}/manifests/{digest}.
func (c *HarborChecker) ImageExists(ctx context.Context, imageRef, digest string) (bool, error) {
	if imageRef == "" || digest == "" {
		return false, nil
	}
	host, name, err := parseRef(imageRef)
	if err != nil {
		return false, fmt.Errorf("image exists: %w", err)
	}
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme, host, name, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("image exists: build request: %w", err)
	}
	resp, err := c.doWithAuthRetry(ctx, req)
	if err != nil {
		return false, fmt.Errorf("image exists HEAD %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyExistence(url, resp.StatusCode)
}

// ReferrerExists checks whether any spec referrer is attached to the subject image.
// Uses the OCI referrers API: GET /v2/{name}/referrers/{digest}.
// Returns true if the response contains at least one referrer manifest.
func (c *HarborChecker) ReferrerExists(ctx context.Context, imageRef, subjectDigest string) (bool, error) {
	if imageRef == "" || subjectDigest == "" {
		return false, nil
	}
	host, name, err := parseRef(imageRef)
	if err != nil {
		return false, fmt.Errorf("referrer exists: %w", err)
	}
	url := fmt.Sprintf("%s://%s/v2/%s/referrers/%s", c.scheme, host, name, subjectDigest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("referrer exists: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")

	resp, err := c.doWithAuthRetry(ctx, req)
	if err != nil {
		return false, fmt.Errorf("referrer exists GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("referrer exists GET %s: indeterminate status %d", url, resp.StatusCode)
	}

	// Parse the referrer index to check if any manifests are listed.
	var idx struct {
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return false, fmt.Errorf("referrer exists GET %s: decode response: %w", url, err)
	}
	return len(idx.Manifests) > 0, nil
}

// PullReachable verifies the image manifest can be fetched (GET, not just HEAD).
// Returns true if the manifest is successfully retrieved.
func (c *HarborChecker) PullReachable(ctx context.Context, imageRef, digest string) (bool, error) {
	if imageRef == "" || digest == "" {
		return false, nil
	}
	host, name, err := parseRef(imageRef)
	if err != nil {
		return false, fmt.Errorf("pull reachable: %w", err)
	}
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme, host, name, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("pull reachable: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")

	resp, err := c.doWithAuthRetry(ctx, req)
	if err != nil {
		return false, fmt.Errorf("pull reachable GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyExistence(url, resp.StatusCode)
}

// classifyExistence turns an HTTP status into the reconcile outcome
// contract: 200 → (true, nil); 404 → (false, nil) confirmed not-found;
// anything else is indeterminate → (false, err).
func classifyExistence(url string, status int) (bool, error) {
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("%s: indeterminate status %d", url, status)
	}
}

// doWithAuthRetry performs req and, if the response is 401 with a usable
// Bearer challenge, transparently retries once with an anonymous token —
// the same flow Client.ResolveTagDigest uses for public registries. A 401
// without a usable challenge, or one that survives the retry, is returned
// as-is so the caller classifies it as indeterminate rather than "not found."
func (c *HarborChecker) doWithAuthRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	//nolint:gosec // G704: req.URL is built in ImageExists/ReferrerExists/PullReachable from
	// the operator-configured registry host and an index.Entry.ImageRef this process itself
	// wrote — not from an untrusted external request, so this is not an SSRF vector.
	resp, err := c.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge, ok := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
	if !ok {
		return resp, nil // no usable challenge; caller sees the 401 as-is
	}
	_ = resp.Body.Close()
	token, tokErr := c.client.anonymousToken(ctx, challenge)
	if tokErr != nil {
		return nil, fmt.Errorf("anonymous token: %w", tokErr)
	}
	retryReq := req.Clone(ctx)
	retryReq.Header.Set("Authorization", "Bearer "+token)
	//nolint:gosec // G704: same trust boundary as the request above (retry of the same URL).
	return c.client.http.Do(retryReq)
}

// parseRef splits "host/project/repo:tag" or "host/project/repo" into (host, name).
// name is everything after the host (project/repo without tag).
func parseRef(imageRef string) (host, name string, err error) {
	slash := strings.SplitN(imageRef, "/", 2)
	if len(slash) != 2 {
		return "", "", fmt.Errorf("invalid image ref (no '/'): %q", imageRef)
	}
	host = slash[0]
	// Strip tag if present.
	name = strings.SplitN(slash[1], ":", 2)[0]
	if name == "" {
		return "", "", fmt.Errorf("invalid image ref (empty name): %q", imageRef)
	}
	return host, name, nil
}
