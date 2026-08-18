// Package registryconfig is the single source of truth for Harbor/OCI
// registry connection settings (scheme, CA trust, auth) shared across
// pkg/oras, pkg/registry, and pkg/build. Before this package existed, each
// of those independently parsed its own env vars and built its own
// HTTP/TLS client, which let them drift onto inconsistent scheme and CA
// trust assumptions.
package registryconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DefaultAddr is the registry host used when NODEVAULT_REGISTRY_ADDR is unset.
// Kept in sync with deploy/03-nodevault.yaml's NODEVAULT_REGISTRY_ADDR and
// infra-lab's CoreDNS mapping (harbor.lab.local -> Cilium LB VIP), the
// standard hostname since Sprint 5 (2026-07-12). The pre-Sprint-5 raw
// nip.io address is stale and unreachable under the current Harbor CA/HTTPRoute.
const DefaultAddr = "harbor.lab.local"

const (
	defaultScheme   = "https"
	defaultAuthFile = "/run/containers/0/auth.json"
)

// containersCertsDir is the standard containers/podman certs.d root that
// Buildah's pull/push path already trusts automatically. It is a package
// variable rather than a constant so tests can point it at a temp directory.
var containersCertsDir = "/etc/containers/certs.d"

// Config holds the registry connection settings shared across NodeVault's
// registry-facing packages (ORAS referrer push, reconcile checks, digest
// resolve, Buildah push).
type Config struct {
	Addr        string
	Scheme      string
	CAFile      string
	AuthFile    string
	InsecureTLS bool
}

// FromEnv builds a Config from environment variables:
//
//	NODEVAULT_REGISTRY_ADDR     — registry host[:port] (default DefaultAddr)
//	NODEVAULT_REGISTRY_SCHEME   — "https" or "http" (default "https")
//	NODEVAULT_REGISTRY_CA_FILE  — CA cert path; falls back to NODEVAULT_ORAS_CA_FILE,
//	                               then to /etc/containers/certs.d/<addr>/ca.crt if present
//	NODEVAULT_ORAS_INSECURE_TLS — "true" skips TLS verification
//	REGISTRY_AUTH_FILE          — Docker/containers auth.json path (default /run/containers/0/auth.json)
func FromEnv() Config {
	addr := os.Getenv("NODEVAULT_REGISTRY_ADDR")
	if addr == "" {
		addr = DefaultAddr
	}
	scheme := os.Getenv("NODEVAULT_REGISTRY_SCHEME")
	if scheme == "" {
		scheme = defaultScheme
	}
	caFile := os.Getenv("NODEVAULT_REGISTRY_CA_FILE")
	if caFile == "" {
		caFile = os.Getenv("NODEVAULT_ORAS_CA_FILE")
	}
	if caFile == "" {
		caFile = discoverCAFile(addr)
	}
	authFile := os.Getenv("REGISTRY_AUTH_FILE")
	if authFile == "" {
		authFile = defaultAuthFile
	}
	return Config{
		Addr:        addr,
		Scheme:      scheme,
		CAFile:      caFile,
		AuthFile:    authFile,
		InsecureTLS: os.Getenv("NODEVAULT_ORAS_INSECURE_TLS") == "true",
	}
}

// discoverCAFile checks the standard containers/podman certs.d layout
// (<containersCertsDir>/<addr>/ca.crt) — the same path a Secret mount
// already feeds to Buildah's pull/push path. Returns "" if no such file
// exists, leaving TLS verification to the process default trust store.
func discoverCAFile(addr string) string {
	path := filepath.Join(containersCertsDir, addr, "ca.crt")
	//nolint:gosec // path is built from containersCertsDir + operator-controlled registry addr, not untrusted input
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// HTTPClient returns an *http.Client configured with c's CA trust settings.
// The system cert pool is preserved (not replaced) so public registries
// (docker.io, ghcr.io, quay.io) keep working alongside a self-signed
// Harbor CA.
func (c Config) HTTPClient() (*http.Client, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: c.InsecureTLS} //nolint:gosec // opt-in via NODEVAULT_ORAS_INSECURE_TLS
	if !c.InsecureTLS && c.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		//nolint:gosec // CAFile is operator-controlled via env var or discovered from containersCertsDir
		pem, readErr := os.ReadFile(c.CAFile)
		if readErr != nil {
			return nil, fmt.Errorf("registryconfig: read CA file %q: %w", c.CAFile, readErr)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("registryconfig: no valid certs found in %q", c.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	// Proxy: http.ProxyFromEnvironment keeps HTTP_PROXY/HTTPS_PROXY support that
	// the default transport provides — a custom transport with a nil Proxy would
	// silently bypass a configured egress proxy for every request.
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tlsConfig,
	}}, nil
}

// Credentials reads the username and password for host from the
// Docker/containers auth.json file at c.AuthFile — the same file Buildah
// uses for pull/push, so no separate credential Secret is required.
// Returns ("", "", nil) when the file is absent or has no entry for host.
func (c Config) Credentials(host string) (username, password string, err error) {
	path := c.AuthFile
	if path == "" {
		path = defaultAuthFile
	}
	//nolint:gosec // path is operator-controlled via REGISTRY_AUTH_FILE or hardcoded default
	data, readErr := os.ReadFile(path)
	if os.IsNotExist(readErr) {
		return "", "", nil
	}
	if readErr != nil {
		return "", "", fmt.Errorf("registryconfig: read auth file %q: %w", path, readErr)
	}
	var parsed struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if parseErr := json.Unmarshal(data, &parsed); parseErr != nil {
		return "", "", fmt.Errorf("registryconfig: parse auth file: %w", parseErr)
	}
	entry, ok := parsed.Auths[host]
	if !ok {
		return "", "", nil
	}
	decoded, decErr := base64.StdEncoding.DecodeString(entry.Auth)
	if decErr != nil {
		return "", "", fmt.Errorf("registryconfig: decode auth for %q: %w", host, decErr)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("registryconfig: auth entry for %q is not user:password format", host)
	}
	return parts[0], parts[1], nil
}
