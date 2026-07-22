package catalogrest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/catalogrest"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func newGCServer(t *testing.T) (*httptest.Server, *index.Store, string) {
	t.Helper()
	store, cat := newTestDeps(t)
	dataCat := catalog.NewDataCatalogAt(t.TempDir())
	svc := catalog.NewToolRegistryService(cat, store)

	resp, err := svc.RegisterTool(context.Background(), &nfv1.RegisterToolRequest{
		ToolName:  "bwa-mem2",
		Version:   "2.2.1",
		Digest:    "sha256:subject-1",
		BuildKind: nfv1.BuildKind_BUILD_KIND_TOOLSPEC,
		ImageUri: "registry.example.com/test:latest",
	})
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	mux := catalogrest.NewMux(store, cat, dataCat)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, store, resp.CasHash
}

func TestListToolProfileGCCandidates_Empty(t *testing.T) {
	ts, _, _ := newGCServer(t)

	resp := doGet(t, ts, ts.URL+"/v1/gc/toolprofile-candidates")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var body catalogrest.ListToolProfileGCCandidatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Candidates) != 0 {
		t.Errorf("expected no candidates, got %d", len(body.Candidates))
	}
}

func TestListToolProfileGCCandidates_BySubjectDigest_ReturnsExcessOnly(t *testing.T) {
	ts, store, casHash := newGCServer(t)

	base := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	for i, digest := range []string{"sha256:p1", "sha256:p2", "sha256:p3", "sha256:p4"} {
		r := index.ToolProfileReferrer{
			Digest:           digest,
			ValidationStatus: "succeeded",
			ValidatedAt:      base.Add(time.Duration(i) * time.Minute),
			ObservedAt:       base.Add(time.Duration(i) * time.Minute),
		}
		if _, err := store.RecordToolProfileReferrer(casHash, &r); err != nil {
			t.Fatalf("RecordToolProfileReferrer(%s): %v", digest, err)
		}
	}

	resp := doGet(t, ts, ts.URL+"/v1/gc/toolprofile-candidates?subject_digest=sha256:subject-1")
	defer func() { _ = resp.Body.Close() }()

	var body catalogrest.ListToolProfileGCCandidatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("len(Candidates) = %d, want 1: %+v", len(body.Candidates), body.Candidates)
	}
	if body.Candidates[0].Digest != "sha256:p1" {
		t.Errorf("candidate digest: got %q, want sha256:p1 (the oldest)", body.Candidates[0].Digest)
	}
	if body.Candidates[0].LifecycleStatus != "GC_CANDIDATE" {
		t.Errorf("lifecycle_status: got %q", body.Candidates[0].LifecycleStatus)
	}
}

func TestListToolProfileGCCandidates_UnknownSubjectDigest_EmptyNotError(t *testing.T) {
	ts, _, _ := newGCServer(t)

	resp := doGet(t, ts, ts.URL+"/v1/gc/toolprofile-candidates?subject_digest=sha256:does-not-exist")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var body catalogrest.ListToolProfileGCCandidatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Candidates) != 0 {
		t.Errorf("expected empty candidates for unknown subject, got %d", len(body.Candidates))
	}
}
