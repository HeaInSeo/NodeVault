package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ResolveTagDigest resolves a "host/name:tag" image reference to its manifest
// digest via the OCI Distribution Spec API.
//
// It is built to reach both an internal Harbor and public registries
// (docker.io, ghcr.io, quay.io) through one code path: it tries HTTPS first,
// transparently performs the standard WWW-Authenticate: Bearer 401 challenge
// -> anonymous token -> retry flow when challenged, and falls back to plain
// HTTP only when the HTTPS connection itself cannot be established. The same
// challenge/retry flow is reused by HarborChecker in checker.go.
func (c *Client) ResolveTagDigest(ctx context.Context, ref string) (string, error) {
	host, name, tag, err := parseDestination(ref)
	if err != nil {
		return "", err
	}

	digest, err := c.resolveTagDigestScheme(ctx, "https", host, name, tag)
	if err == nil {
		return digest, nil
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return "", err
	}
	return c.resolveTagDigestScheme(ctx, "http", host, name, tag)
}

func (c *Client) resolveTagDigestScheme(ctx context.Context, scheme, host, name, tag string) (string, error) {
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, host, name, tag)

	var lastErr error
	for _, accept := range acceptHeaders {
		digest, err := c.fetchManifestDigest(ctx, manifestURL, accept)
		if err == nil {
			return digest, nil
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return "", err // transport failure: let the caller try the other scheme
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
		return d, nil
	}
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if jerr := json.NewDecoder(resp.Body).Decode(&m); jerr == nil && m.Config.Digest != "" {
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
