package registry

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/registryconfig"
)

// setRegistryEnv points registryconfig.FromEnv() (consumed internally by
// ResolveTagDigest) at a known, isolated configuration for the duration of
// the test: an addr that can't collide with any real /etc/containers/certs.d
// entry on the test host, the given scheme, and caFile ("" means "no CA
// override — rely on the system trust store only").
func setRegistryEnv(t *testing.T, scheme, caFile string) {
	t.Helper()
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "registry-resolve-test.invalid")
	t.Setenv("NODEVAULT_REGISTRY_SCHEME", scheme)
	t.Setenv("NODEVAULT_REGISTRY_CA_FILE", caFile)
	t.Setenv("NODEVAULT_ORAS_CA_FILE", "")
	t.Setenv("NODEVAULT_ORAS_INSECURE_TLS", "")
	t.Setenv("REGISTRY_AUTH_FILE", filepath.Join(t.TempDir(), "auth.json"))
}

// writeCAFile PEM-encodes certs into a single temp file suitable for
// NODEVAULT_REGISTRY_CA_FILE.
func writeCAFile(t *testing.T, certs ...*x509.Certificate) string {
	t.Helper()
	var buf bytes.Buffer
	for _, cert := range certs {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			t.Fatalf("pem encode: %v", err)
		}
	}
	f := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return f
}

// ─── ResolveTagDigest: direct HTTPS, trusted via registryconfig CA ──────────

func TestResolveTagDigest_DirectHTTPS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	setRegistryEnv(t, "https", writeCAFile(t, ts.Certificate()))

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "https://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	want := "sha256:" + strings.Repeat("a", 64)
	if digest != want {
		t.Errorf("got %q, want %q", digest, want)
	}
}

// ─── ResolveTagDigest: WWW-Authenticate: Bearer challenge ────────────────────

func TestResolveTagDigest_BearerChallenge(t *testing.T) {
	authTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("service"); got != "registry.example.com" {
			t.Errorf("auth request service param: got %q", got)
		}
		_, _ = fmt.Fprintln(w, `{"token":"anon-token-123"}`)
	}))
	defer authTS.Close()

	registryTS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anon-token-123" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm=%q,service="registry.example.com",scope="repository:lib/img:pull"`, authTS.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("b", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer registryTS.Close()

	// Both servers present distinct self-signed certs; trust both via the
	// same registryconfig CA file mechanism ResolveTagDigest now uses.
	setRegistryEnv(t, "https", writeCAFile(t, registryTS.Certificate(), authTS.Certificate()))

	c := NewClient()
	host := strings.TrimPrefix(registryTS.URL, "https://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/lib/img:pull")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	want := "sha256:" + strings.Repeat("b", 64)
	if digest != want {
		t.Errorf("got %q, want %q", digest, want)
	}
}

func TestResolveTagDigest_BearerChallengeWithoutAuthHeader_Errors(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	setRegistryEnv(t, "https", writeCAFile(t, ts.Certificate()))

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "https://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	_, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatal("expected error for 401 without a Bearer challenge")
	}
}

// ─── ResolveTagDigest: no more silent HTTP downgrade on TLS failure ─────────
//
// Before the fix, ResolveTagDigest tried HTTPS first and fell back to plain
// HTTP on *any* *url.Error, which includes TLS certificate-verification
// failures. An on-path attacker could force that downgrade (e.g. by
// resetting/breaking the TLS handshake) and hand back an arbitrary digest
// over the now-unauthenticated connection. These tests prove that a TLS
// verification failure now fails closed instead of silently retrying in
// plaintext.

func TestResolveTagDigest_TLSVerificationFailure_DoesNotFallBackToHTTP(t *testing.T) {
	// This handler would happily hand back a digest if reached in plaintext.
	// If ResolveTagDigest silently downgraded to HTTP against this same
	// address, it would receive this digest instead of failing.
	plaintextDigest := "sha256:" + strings.Repeat("c", 64)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", plaintextDigest)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// No CA configured for ts's self-signed certificate: the TLS handshake
	// will fail certificate verification.
	setRegistryEnv(t, "https", "")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "https://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatalf("expected error for untrusted TLS certificate, got digest %q", digest)
	}
	if digest == plaintextDigest {
		t.Fatalf("ResolveTagDigest returned the plaintext-reachable digest %q — it fell back to HTTP", digest)
	}
	var certErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	if !errors.As(err, &certErr) && !errors.As(err, &hostnameErr) &&
		!strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("expected a TLS/certificate verification error, got: %v", err)
	}
}

func TestResolveTagDigest_InsecureTLSOptIn_StillWorks(t *testing.T) {
	// Sanity check that the registryconfig plumbing itself works end-to-end:
	// an operator who explicitly opts into NODEVAULT_ORAS_INSECURE_TLS can
	// still resolve against a self-signed cert without configuring a CA file.
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("d", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	setRegistryEnv(t, "https", "")
	t.Setenv("NODEVAULT_ORAS_INSECURE_TLS", "true")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "https://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	want := "sha256:" + strings.Repeat("d", 64)
	if digest != want {
		t.Errorf("got %q, want %q", digest, want)
	}
}

// ─── ResolveTagDigest: explicit HTTP opt-in via NODEVAULT_REGISTRY_SCHEME ───

func TestResolveTagDigest_PlainHTTP_ExplicitSchemeOptIn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("e", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	setRegistryEnv(t, "http", "")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "http://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	want := "sha256:" + strings.Repeat("e", 64)
	if digest != want {
		t.Errorf("got %q, want %q", digest, want)
	}
}

func TestResolveTagDigest_HTTPSDefault_DoesNotReachPlainHTTPServer(t *testing.T) {
	// With no NODEVAULT_REGISTRY_SCHEME override, the default scheme is
	// https. Pointed at a plain-HTTP server, the request must fail rather
	// than silently trying HTTP.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("f", 64))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	setRegistryEnv(t, "", "")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "http://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	_, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatal("expected error when https (default scheme) is used against a plain-http test server")
	}
}

// ─── ResolveTagDigest: malformed digest is rejected, not passed through ─────

func TestResolveTagDigest_MalformedDigestHeader_Rejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "'; DROP TABLE tools;--")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	setRegistryEnv(t, "http", "")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "http://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatalf("expected error for malformed digest, got digest %q", digest)
	}
	if digest != "" {
		t.Errorf("expected empty digest on error, got %q", digest)
	}
}

func TestResolveTagDigest_MalformedDigestInBody_Rejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Docker-Content-Digest header: falls through to parsing the
		// manifest body's config.digest field.
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"config":{"digest":"not-a-real-digest"}}`)
	}))
	defer ts.Close()

	setRegistryEnv(t, "http", "")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "http://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatalf("expected error for malformed config.digest, got digest %q", digest)
	}
	if digest != "" {
		t.Errorf("expected empty digest on error, got %q", digest)
	}
}

func TestResolveTagDigest_ShortHexDigest_Rejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// sha256 digest with the right prefix but wrong length.
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	setRegistryEnv(t, "http", "")

	c := NewClient()
	host := strings.TrimPrefix(ts.URL, "http://")
	// The reference host IS the configured registry for this test, so the
	// operator scheme/CA/InsecureTLS settings apply (clientForHost scopes
	// those to cfg.Addr only).
	t.Setenv("NODEVAULT_REGISTRY_ADDR", host)

	_, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatal("expected error for short sha256 digest")
	}
}

// ─── ResolveTagDigest: invalid input ──────────────────────────────────────────

func TestResolveTagDigest_InvalidRef(t *testing.T) {
	c := NewClient()
	_, err := c.ResolveTagDigest(context.Background(), "noslashnocolon")
	if err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

// ─── validateDigestFormat ────────────────────────────────────────────────────

func TestValidateDigestFormat(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	if err := validateDigestFormat(valid); err != nil {
		t.Errorf("expected valid sha256 digest to pass, got: %v", err)
	}

	cases := []string{
		"",
		"not-a-digest",
		"sha256:",
		"sha256:short",
		"sha256:" + strings.Repeat("g", 64), // 'g' is not hex
		"sha256:" + strings.Repeat("A", 64), // uppercase hex not allowed
		"sha256:" + strings.Repeat("a", 63), // too short
		"sha256:" + strings.Repeat("a", 65), // too long
		"; rm -rf /",
	}
	for _, c := range cases {
		if err := validateDigestFormat(c); err == nil {
			t.Errorf("expected %q to be rejected as malformed", c)
		}
	}
}

// ─── parseBearerChallenge ──────────────────────────────────────────────────────

func TestParseBearerChallenge_Valid(t *testing.T) {
	ch, ok := parseBearerChallenge(`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:lib/img:pull"`)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ch.realm != "https://auth.example.com/token" || ch.service != "registry.example.com" || ch.scope != "repository:lib/img:pull" {
		t.Errorf("unexpected challenge: %+v", ch)
	}
}

func TestParseBearerChallenge_NotBearer(t *testing.T) {
	_, ok := parseBearerChallenge(`Basic realm="example"`)
	if ok {
		t.Fatal("expected ok=false for non-Bearer scheme")
	}
}

func TestParseBearerChallenge_MissingRealm(t *testing.T) {
	_, ok := parseBearerChallenge(`Bearer service="registry.example.com"`)
	if ok {
		t.Fatal("expected ok=false when realm is missing")
	}
}

// transportInsecure reports whether c's transport skips TLS verification.
func transportInsecure(t *testing.T, c *http.Client) bool {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil {
		return false
	}
	return tr.TLSClientConfig.InsecureSkipVerify
}

// TestClientForHost_ExternalHostIgnoresRegistryOverrides is the regression test
// for the P1 finding on #66: the operator's configured scheme / CA / InsecureTLS
// overrides (meant for the internal Harbor at cfg.Addr) must NOT be applied to a
// caller-controlled external host. Otherwise an install that opted its Harbor
// into http / NODEVAULT_ORAS_INSECURE_TLS=true would also resolve a submitted
// docker.io base-image reference over plaintext / unverifiable TLS and accept an
// attacker-supplied digest.
func TestClientForHost_ExternalHostIgnoresRegistryOverrides(t *testing.T) {
	// Operator opted the internal Harbor into http + insecure TLS.
	cfg := registryconfig.Config{Addr: "harbor.lab.local", Scheme: "http", InsecureTLS: true}

	// Configured host: the operator's opt-in applies.
	scheme, client, err := clientForHost(cfg, "harbor.lab.local")
	if err != nil {
		t.Fatalf("clientForHost(configured host): %v", err)
	}
	if scheme != "http" {
		t.Errorf("configured host scheme: got %q want http (operator opt-in must apply to cfg.Addr)", scheme)
	}
	if !transportInsecure(t, client) {
		t.Error("configured host: expected InsecureTLS opt-in to apply to cfg.Addr")
	}

	// External, caller-controlled host: overrides MUST NOT leak here.
	scheme, client, err = clientForHost(cfg, "docker.io")
	if err != nil {
		t.Fatalf("clientForHost(external host): %v", err)
	}
	if scheme != "https" {
		t.Errorf("external host scheme: got %q want https (internal Harbor's http opt-in leaked to external host)", scheme)
	}
	if transportInsecure(t, client) {
		t.Error("external host: TLS verification was skipped (internal Harbor's InsecureTLS opt-in leaked to external host)")
	}
}
