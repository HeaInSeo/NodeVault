package oras

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryFromRepo(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"harbor.lab.local/library/mytool", "harbor.lab.local"},
		{"harbor.lab.local/library/mytool:latest", "harbor.lab.local"},
		{"harbor.lab.local", "harbor.lab.local"},
		{"10.96.0.1:5000/library/tool", "10.96.0.1:5000"},
	}
	for _, c := range cases {
		if got := registryFromRepo(c.in); got != c.want {
			t.Errorf("registryFromRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCredentialsFromAuthFile_FileAbsent(t *testing.T) {
	t.Setenv("REGISTRY_AUTH_FILE", filepath.Join(t.TempDir(), "nonexistent.json"))
	u, p, err := credentialsFromAuthFile("harbor.lab.local")
	if err != nil {
		t.Fatalf("expected nil error for absent file, got %v", err)
	}
	if u != "" || p != "" {
		t.Errorf("expected empty credentials, got user=%q pass=%q", u, p)
	}
}

func TestCredentialsFromAuthFile_RegistryNotInFile(t *testing.T) {
	f := writeAuthJSON(t, "other.registry.io", "admin", "secret")
	t.Setenv("REGISTRY_AUTH_FILE", f)

	u, p, err := credentialsFromAuthFile("harbor.lab.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "" || p != "" {
		t.Errorf("expected empty credentials for unknown registry, got user=%q", u)
	}
}

func TestCredentialsFromAuthFile_ValidEntry(t *testing.T) {
	f := writeAuthJSON(t, "harbor.lab.local", "admin", "Harbor12345")
	t.Setenv("REGISTRY_AUTH_FILE", f)

	u, p, err := credentialsFromAuthFile("harbor.lab.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "admin" || p != "Harbor12345" {
		t.Errorf("got user=%q pass=%q, want admin/Harbor12345", u, p)
	}
}

func TestCredentialsFromAuthFile_MultipleRegistries(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "auth.json")
	content := fmt.Sprintf(`{"auths":{%q:{"auth":%q},%q:{"auth":%q}}}`,
		"harbor.lab.local", base64.StdEncoding.EncodeToString([]byte("admin:Harbor12345")),
		"ghcr.io", base64.StdEncoding.EncodeToString([]byte("user:token")),
	)
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	t.Setenv("REGISTRY_AUTH_FILE", f)

	u, p, err := credentialsFromAuthFile("ghcr.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != "user" || p != "token" {
		t.Errorf("got user=%q pass=%q, want user/token", u, p)
	}
}

func TestCredentialsFromAuthFile_MalformedJSON(t *testing.T) {
	f := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(f, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("REGISTRY_AUTH_FILE", f)

	_, _, err := credentialsFromAuthFile("harbor.lab.local")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// writeAuthJSON writes a single-entry auth.json and returns its path.
func writeAuthJSON(t *testing.T, registry, user, pass string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "auth.json")
	encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	content := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registry, encoded)
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return f
}
