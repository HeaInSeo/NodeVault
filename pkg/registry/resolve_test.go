package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── ResolveTagDigest: direct HTTPS, no auth required ────────────────────────

func TestResolveTagDigest_DirectHTTPS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:tlshash")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := newClientWithHTTP(ts.Client())
	host := strings.TrimPrefix(ts.URL, "https://")

	digest, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	if digest != "sha256:tlshash" {
		t.Errorf("got %q, want %q", digest, "sha256:tlshash")
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
		w.Header().Set("Docker-Content-Digest", "sha256:authed")
		w.WriteHeader(http.StatusOK)
	}))
	defer registryTS.Close()

	// Both servers present distinct self-signed certs; trust both.
	pool := x509.NewCertPool()
	pool.AddCert(registryTS.Certificate())
	pool.AddCert(authTS.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}

	c := newClientWithHTTP(client)
	host := strings.TrimPrefix(registryTS.URL, "https://")

	digest, err := c.ResolveTagDigest(context.Background(), host+"/lib/img:pull")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	if digest != "sha256:authed" {
		t.Errorf("got %q, want %q", digest, "sha256:authed")
	}
}

func TestResolveTagDigest_BearerChallengeWithoutAuthHeader_Errors(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	c := newClientWithHTTP(ts.Client())
	host := strings.TrimPrefix(ts.URL, "https://")

	_, err := c.ResolveTagDigest(context.Background(), host+"/img:latest")
	if err == nil {
		t.Fatal("expected error for 401 without a Bearer challenge")
	}
}

// ─── ResolveTagDigest: falls back to plain HTTP when HTTPS is unavailable ────

func TestResolveTagDigest_FallsBackToPlainHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:plainhash")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := newClientWithHTTP(ts.Client())
	host := strings.TrimPrefix(ts.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	digest, err := c.ResolveTagDigest(ctx, host+"/img:latest")
	if err != nil {
		t.Fatalf("ResolveTagDigest: %v", err)
	}
	if digest != "sha256:plainhash" {
		t.Errorf("got %q, want %q", digest, "sha256:plainhash")
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
