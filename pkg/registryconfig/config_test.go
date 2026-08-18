package registryconfig

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func withTempCertsDir(t *testing.T) {
	t.Helper()
	orig := containersCertsDir
	containersCertsDir = t.TempDir()
	t.Cleanup(func() { containersCertsDir = orig })
}

func TestRegistryConfig_FromEnv_Defaults(t *testing.T) {
	withTempCertsDir(t)
	t.Setenv("NODEVAULT_REGISTRY_ADDR", "")
	t.Setenv("NODEVAULT_REGISTRY_SCHEME", "")
	t.Setenv("NODEVAULT_REGISTRY_CA_FILE", "")
	t.Setenv("NODEVAULT_ORAS_CA_FILE", "")
	t.Setenv("NODEVAULT_ORAS_INSECURE_TLS", "")
	t.Setenv("REGISTRY_AUTH_FILE", "")

	cfg := FromEnv()

	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.Scheme != "https" {
		t.Errorf("Scheme = %q, want %q", cfg.Scheme, "https")
	}
	if cfg.CAFile != "" {
		t.Errorf("CAFile = %q, want empty (no discovery in temp certs dir)", cfg.CAFile)
	}
	if cfg.AuthFile != defaultAuthFile {
		t.Errorf("AuthFile = %q, want %q", cfg.AuthFile, defaultAuthFile)
	}
	if cfg.InsecureTLS {
		t.Error("InsecureTLS = true, want false")
	}
}

func TestRegistryConfig_DiscoverCAFile(t *testing.T) {
	withTempCertsDir(t)
	addr := "harbor.lab.local"

	if got := discoverCAFile(addr); got != "" {
		t.Errorf("discoverCAFile before file exists = %q, want empty", got)
	}

	certDir := filepath.Join(containersCertsDir, addr)
	if err := os.MkdirAll(certDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	caPath := filepath.Join(certDir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("dummy-cert"), 0o600); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}

	if got := discoverCAFile(addr); got != caPath {
		t.Errorf("discoverCAFile after file exists = %q, want %q", got, caPath)
	}
}

func TestRegistryConfig_ORASCAFileFallback(t *testing.T) {
	withTempCertsDir(t)
	t.Setenv("NODEVAULT_REGISTRY_CA_FILE", "")
	t.Setenv("NODEVAULT_ORAS_CA_FILE", "/path/to/oras-ca.pem")

	cfg := FromEnv()

	if cfg.CAFile != "/path/to/oras-ca.pem" {
		t.Errorf("CAFile = %q, want fallback to NODEVAULT_ORAS_CA_FILE", cfg.CAFile)
	}
}

func TestRegistryConfig_ExplicitCAFileWinsOverORASFallback(t *testing.T) {
	withTempCertsDir(t)
	t.Setenv("NODEVAULT_REGISTRY_CA_FILE", "/explicit/ca.pem")
	t.Setenv("NODEVAULT_ORAS_CA_FILE", "/path/to/oras-ca.pem")

	cfg := FromEnv()

	if cfg.CAFile != "/explicit/ca.pem" {
		t.Errorf("CAFile = %q, want explicit NODEVAULT_REGISTRY_CA_FILE to win", cfg.CAFile)
	}
}

func TestConfig_Credentials_FileAbsent(t *testing.T) {
	cfg := Config{AuthFile: filepath.Join(t.TempDir(), "nonexistent.json")}
	u, p, err := cfg.Credentials("harbor.lab.local")
	if err != nil {
		t.Fatalf("expected nil error for absent file, got %v", err)
	}
	if u != "" || p != "" {
		t.Errorf("expected empty credentials, got user=%q pass=%q", u, p)
	}
}

func TestConfig_Credentials_HostNotInFile(t *testing.T) {
	cfg := Config{AuthFile: writeAuthJSON(t, "other.registry.io", "admin", "secret")}
	u, p, err := cfg.Credentials("harbor.lab.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "" || p != "" {
		t.Errorf("expected empty credentials for unknown host, got user=%q", u)
	}
}

func TestConfig_Credentials_ValidEntry(t *testing.T) {
	cfg := Config{AuthFile: writeAuthJSON(t, "harbor.lab.local", "admin", "Harbor12345")}
	u, p, err := cfg.Credentials("harbor.lab.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "admin" || p != "Harbor12345" {
		t.Errorf("got user=%q pass=%q, want admin/Harbor12345", u, p)
	}
}

func TestConfig_Credentials_MultipleRegistries(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "auth.json")
	content := fmt.Sprintf(`{"auths":{%q:{"auth":%q},%q:{"auth":%q}}}`,
		"harbor.lab.local", base64.StdEncoding.EncodeToString([]byte("admin:Harbor12345")),
		"ghcr.io", base64.StdEncoding.EncodeToString([]byte("user:token")),
	)
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	cfg := Config{AuthFile: f}
	u, p, err := cfg.Credentials("ghcr.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "user" || p != "token" {
		t.Errorf("got user=%q pass=%q, want user/token", u, p)
	}
}

func TestConfig_Credentials_MalformedJSON(t *testing.T) {
	f := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(f, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := Config{AuthFile: f}
	_, _, err := cfg.Credentials("harbor.lab.local")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// writeAuthJSON writes a single-entry auth.json and returns its path.
func writeAuthJSON(t *testing.T, host, user, pass string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "auth.json")
	encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	content := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, encoded)
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return f
}

// TestConfig_HTTPClient_PreservesProxyFromEnvironment is the regression test for
// the P2 finding on #66: the custom transport built by HTTPClient() dropped
// http.ProxyFromEnvironment, so in environments where public-registry egress
// requires HTTP_PROXY/HTTPS_PROXY every request (including the bearer-token
// request) bypassed the proxy and failed. The transport must keep proxy support.
func TestConfig_HTTPClient_PreservesProxyFromEnvironment(t *testing.T) {
	client, err := Config{}.HTTPClient()
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type: got %T", client.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("HTTPClient transport has a nil Proxy — HTTP_PROXY/HTTPS_PROXY would be ignored")
	}
	if reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Error("HTTPClient transport Proxy is not http.ProxyFromEnvironment")
	}
}
