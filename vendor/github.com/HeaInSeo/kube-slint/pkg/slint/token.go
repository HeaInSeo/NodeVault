package slint

import (
	"fmt"
	"os"
	"strings"
)

// DefaultTokenPath is the projected service account token path inside a Kubernetes pod.
const DefaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// ReadServiceAccountToken reads a bearer token from path.
// Use this in your E2E test to populate SessionConfig.Token:
//
//	token, err := slint.ReadServiceAccountToken("/path/to/token")
//	if err != nil { t.Fatal(err) }
//	sess := slint.NewSession(slint.SessionConfig{Token: token, ...})
func ReadServiceAccountToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("slint: reading service account token from %q: %w", path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("slint: service account token file %q is empty", path)
	}
	return tok, nil
}

// ReadServiceAccountTokenFromEnv reads the token from the environment variable named by envVar,
// falling back to reading from path if the variable is unset or empty.
//
// Typical usage in CI (token injected as a secret env var):
//
//	token, err := slint.ReadServiceAccountTokenFromEnv("SLINT_SA_TOKEN", "")
func ReadServiceAccountTokenFromEnv(envVar, fallbackPath string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v, nil
	}
	if fallbackPath == "" {
		return "", fmt.Errorf("slint: env var %q is unset and no fallback path provided", envVar)
	}
	return ReadServiceAccountToken(fallbackPath)
}
