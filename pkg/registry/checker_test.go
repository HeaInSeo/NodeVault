package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHarborChecker_ImageExists_404_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	checker := &HarborChecker{client: newClientWithHTTP(ts.Client()), scheme: "http"}

	ok, err := checker.ImageExists(context.Background(), host+"/library/tool:latest", "sha256:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for 404")
	}
}

func TestHarborChecker_ImageExists_401_IsNotNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No WWW-Authenticate header -> no usable challenge -> 401 passes through.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	checker := &HarborChecker{client: newClientWithHTTP(ts.Client()), scheme: "http"}

	ok, err := checker.ImageExists(context.Background(), host+"/library/tool:latest", "sha256:abc")
	if err == nil {
		t.Fatal("expected error for 401, got nil (401 must not be classified as not-found)")
	}
	if ok {
		t.Error("expected ok=false for 401")
	}
}

func TestHarborChecker_ImageExists_401WithChallenge_RetriesWithAnonymousToken(t *testing.T) {
	mux := http.NewServeMux()
	registryTS := httptest.NewServer(mux)
	defer registryTS.Close()

	authTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"anon-token"}`))
	}))
	defer authTS.Close()

	mux.HandleFunc("/v2/library/tool/manifests/sha256:abc", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer anon-token" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="registry"`, authTS.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})

	host := strings.TrimPrefix(registryTS.URL, "http://")
	checker := &HarborChecker{client: newClientWithHTTP(registryTS.Client()), scheme: "http"}

	ok, err := checker.ImageExists(context.Background(), host+"/library/tool:latest", "sha256:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ok=true after anonymous token retry")
	}
}

func TestHarborChecker_UsesConfiguredScheme(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")

	httpChecker := &HarborChecker{client: newClientWithHTTP(ts.Client()), scheme: "http"}
	if ok, err := httpChecker.ImageExists(context.Background(), host+"/library/tool:latest", "sha256:abc"); err != nil || !ok {
		t.Fatalf("scheme=http against a plain-http test server: ok=%v err=%v", ok, err)
	}

	httpsChecker := &HarborChecker{client: newClientWithHTTP(ts.Client()), scheme: "https"}
	if _, err := httpsChecker.ImageExists(context.Background(), host+"/library/tool:latest", "sha256:abc"); err == nil {
		t.Fatal("expected error when scheme=https is used against a plain-http test server")
	}
}

func TestHarborChecker_ReferrerExists_500_IsIndeterminate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	checker := &HarborChecker{client: newClientWithHTTP(ts.Client()), scheme: "http"}

	ok, err := checker.ReferrerExists(context.Background(), host+"/library/tool:latest", "sha256:abc")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if ok {
		t.Error("expected ok=false for 500")
	}
}

func TestHarborChecker_PullReachable_200_ReturnsTrue(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "http://")
	checker := &HarborChecker{client: newClientWithHTTP(ts.Client()), scheme: "http"}

	ok, err := checker.PullReachable(context.Background(), host+"/library/tool:latest", "sha256:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ok=true for 200")
	}
}
