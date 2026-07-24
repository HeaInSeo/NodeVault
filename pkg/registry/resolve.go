package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/HeaInSeo/NodeVault/pkg/registryconfig"
)

// digestPattern matches the general OCI content digest shape:
// "<algorithm>:<encoded>", per the OCI Image Spec digest grammar
// (https://github.com/opencontainers/image-spec/blob/main/descriptor.md#digests).
var digestPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)

// sha256DigestPattern additionally constrains the common sha256 case to
// exactly 64 lowercase hex characters — the only algorithm NodeVault's own
// build/push path produces or trusts.
var sha256DigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// validateDigestFormat rejects a value that does not have the shape of an
// OCI content digest. Without this check, a misbehaving/compromised
// registry or on-path proxy could hand back an arbitrary string in the
// Docker-Content-Digest header or manifest body that would flow through
// unchecked as "the resolved digest."
func validateDigestFormat(d string) error {
	if !digestPattern.MatchString(d) {
		return fmt.Errorf("malformed digest %q: does not match \"algorithm:encoded\" format", d)
	}
	if strings.HasPrefix(d, "sha256:") && !sha256DigestPattern.MatchString(d) {
		return fmt.Errorf("malformed digest %q: sha256 digest must be exactly 64 lowercase hex characters", d)
	}
	return nil
}

// ResolveTagDigest resolves a "host/name:tag" image reference to its manifest
// digest via the OCI Distribution Spec API.
//
// Scheme and TLS/CA trust come from registryconfig.Config
// (registryconfig.FromEnv()) — the same shared settings HarborChecker
// (checker.go) and pkg/oras's newRemoteRepository already use, so this
// resolve path no longer trusts a different CA or scheme than the rest of
// NodeVault. It transparently performs the standard WWW-Authenticate:
// Bearer 401 challenge -> anonymous token -> retry flow when challenged
// (the same flow reused by HarborChecker), but it is otherwise a single,
// deterministic attempt: there is no probe-HTTPS-then-silently-retry-over-
// HTTP fallback. Retrying in plaintext on any HTTPS failure — including a
// TLS certificate verification failure — would let an on-path attacker
// force the downgrade (by breaking the TLS handshake) and then hand back
// an arbitrary digest over an unauthenticated connection with no further
// check. Operators who need to reach a plain-HTTP registry (e.g. a kind
// test registry) opt in explicitly via NODEVAULT_REGISTRY_SCHEME=http.
func (*Client) ResolveTagDigest(ctx context.Context, ref string) (string, error) {
	host, name, tag, err := parseDestination(ref)
	if err != nil {
		return "", err
	}

	cfg := registryconfig.FromEnv()
	httpClient, err := cfg.HTTPClient()
	if err != nil {
		return "", fmt.Errorf("registry: build HTTP client: %w", err)
	}
	rc := newClientWithHTTP(httpClient)

	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", cfg.Scheme, host, name, tag)
	var lastErr error
	for _, accept := range acceptHeaders {
		digest, err := rc.fetchManifestDigest(ctx, manifestURL, accept)
		if err == nil {
			return digest, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("digest not found in registry response for %s", manifestURL)
}

// fetchManifestDigest performs one GET, transparently handling a single
// WWW-Authenticate: Bearer challenge if the registry requires anonymous
// token auth.
func (c *Client) fetchManifestDigest(ctx context.Context, manifestURL, accept string) (string, error) {
	resp, err := c.getManifest(ctx, manifestURL, accept, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		challenge, ok := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
		if !ok {
			return "", fmt.Errorf("registry GET %s: 401 without a Bearer challenge", manifestURL)
		}
		token, terr := c.anonymousToken(ctx, challenge)
		if terr != nil {
			return "", fmt.Errorf("registry GET %s: anonymous token: %w", manifestURL, terr)
		}
		resp, err = c.getManifest(ctx, manifestURL, accept, token)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry GET %s: status %d", manifestURL, resp.StatusCode)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		if verr := validateDigestFormat(d); verr != nil {
			return "", fmt.Errorf("registry GET %s: Docker-Content-Digest header: %w", manifestURL, verr)
		}
		return d, nil
	}
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if jerr := json.NewDecoder(resp.Body).Decode(&m); jerr == nil && m.Config.Digest != "" {
		if verr := validateDigestFormat(m.Config.Digest); verr != nil {
			return "", fmt.Errorf("registry GET %s: manifest config.digest: %w", manifestURL, verr)
		}
		return m.Config.Digest, nil
	}
	return "", fmt.Errorf("digest not found in registry response for %s", manifestURL)
}

func (c *Client) getManifest(ctx context.Context, manifestURL, accept, bearerToken string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", accept)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry GET %s: %w", manifestURL, err)
	}
	return resp, nil
}

type bearerChallenge struct {
	realm, service, scope string
}

// parseBearerChallenge parses a WWW-Authenticate header of the form:
//
//	Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:name:pull"
func parseBearerChallenge(header string) (bearerChallenge, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return bearerChallenge{}, false
	}
	var ch bearerChallenge
	for _, part := range strings.Split(header[len(prefix):], ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		val := strings.Trim(kv[1], `"`)
		switch kv[0] {
		case "realm":
			ch.realm = val
		case "service":
			ch.service = val
		case "scope":
			ch.scope = val
		}
	}
	if ch.realm == "" {
		return bearerChallenge{}, false
	}
	return ch, true
}

func (c *Client) anonymousToken(ctx context.Context, ch bearerChallenge) (string, error) {
	u, err := url.Parse(ch.realm)
	if err != nil {
		return "", fmt.Errorf("parse auth realm %q: %w", ch.realm, err)
	}
	q := u.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	if ch.scope != "" {
		q.Set("scope", ch.scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build auth request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth GET %s: %w", u.String(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth server %s returned status %d", u.String(), resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if jerr := json.NewDecoder(resp.Body).Decode(&tok); jerr != nil {
		return "", fmt.Errorf("decode auth response: %w", jerr)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", fmt.Errorf("auth response from %s missing token", u.String())
}
