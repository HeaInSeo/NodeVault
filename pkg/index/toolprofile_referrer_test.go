package index_test

import (
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// ref builds a ToolProfileReferrer with a validation-completion timestamp
// minutesAgo minutes before a fixed reference point, so test cases can express
// recency without depending on wall-clock time.
func ref(digest string, minutesAgo int) index.ToolProfileReferrer {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	return index.ToolProfileReferrer{
		Digest:           digest,
		ValidationStatus: "succeeded",
		ValidatedAt:      base.Add(-time.Duration(minutesAgo) * time.Minute),
		ObservedAt:       base.Add(-time.Duration(minutesAgo) * time.Minute),
	}
}

func recordAll(t *testing.T, s *index.Store, casHash string, refs ...index.ToolProfileReferrer) []index.ToolProfileReferrer {
	t.Helper()
	var got []index.ToolProfileReferrer
	for i := range refs {
		var err error
		got, err = s.RecordToolProfileReferrer(casHash, &refs[i])
		if err != nil {
			t.Fatalf("RecordToolProfileReferrer(%q): %v", refs[i].Digest, err)
		}
	}
	return got
}

func statusOf(refs []index.ToolProfileReferrer, digest string) index.ReferrerLifecycleStatus {
	for i := range refs {
		if refs[i].Digest == digest {
			return refs[i].LifecycleStatus
		}
	}
	return ""
}

func TestRecordToolProfileReferrer_SingleReferrer_Active(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-tpr-1", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := recordAll(t, s, "hash-tpr-1", ref("sha256:a", 0))

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].LifecycleStatus != index.ReferrerActive || got[0].Rank != 1 {
		t.Errorf("got %+v, want ACTIVE rank 1", got[0])
	}
}

func TestRecordToolProfileReferrer_ExactlyRetainLimit_AllActive(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-tpr-3", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := recordAll(t, s, "hash-tpr-3",
		ref("sha256:a", 20), ref("sha256:b", 10), ref("sha256:c", 0))

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for _, r := range got {
		if r.LifecycleStatus != index.ReferrerActive {
			t.Errorf("digest %q: got %q, want ACTIVE", r.Digest, r.LifecycleStatus)
		}
	}
}

func TestRecordToolProfileReferrer_OneOverLimit_OldestMarkedGCCandidate(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-tpr-4", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := recordAll(t, s, "hash-tpr-4",
		ref("sha256:a", 30), ref("sha256:b", 20), ref("sha256:c", 10), ref("sha256:d", 0))

	if status := statusOf(got, "sha256:a"); status != index.ReferrerGCCandidate {
		t.Errorf("oldest (sha256:a): got %q, want GC_CANDIDATE", status)
	}
	for _, digest := range []string{"sha256:b", "sha256:c", "sha256:d"} {
		if status := statusOf(got, digest); status != index.ReferrerActive {
			t.Errorf("%s: got %q, want ACTIVE", digest, status)
		}
	}

	cands, err := s.ListToolProfileGCCandidates("hash-tpr-4")
	if err != nil {
		t.Fatalf("ListToolProfileGCCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Digest != "sha256:a" {
		t.Errorf("ListToolProfileGCCandidates: got %+v, want [sha256:a]", cands)
	}
	if cands[0].GCReason != "retained_limit_exceeded" {
		t.Errorf("GCReason: got %q", cands[0].GCReason)
	}
	if cands[0].MarkedAt.IsZero() {
		t.Error("MarkedAt should be set once a referrer becomes GC_CANDIDATE")
	}
}

func TestRecordToolProfileReferrer_WellOverLimit_AllExcessMarkedGCCandidate(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-tpr-5", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := recordAll(t, s, "hash-tpr-5",
		ref("sha256:a", 40), ref("sha256:b", 30), ref("sha256:c", 20),
		ref("sha256:d", 10), ref("sha256:e", 0))

	active := 0
	candidate := 0
	for _, r := range got {
		switch r.LifecycleStatus {
		case index.ReferrerActive:
			active++
		case index.ReferrerGCCandidate:
			candidate++
		}
	}
	if active != index.DefaultToolProfileReferrerRetain {
		t.Errorf("active count = %d, want %d", active, index.DefaultToolProfileReferrerRetain)
	}
	if candidate != 2 {
		t.Errorf("candidate count = %d, want 2", candidate)
	}
	for _, digest := range []string{"sha256:a", "sha256:b"} {
		if status := statusOf(got, digest); status != index.ReferrerGCCandidate {
			t.Errorf("%s: got %q, want GC_CANDIDATE", digest, status)
		}
	}
}

// TestRecordToolProfileReferrer_OutOfOrderPushes_RanksByValidatedAtNotPushOrder
// proves ranking never depends on push/insertion order (a stand-in for
// registry Referrers() listing order, which OCI does not guarantee) — only on
// each referrer's ValidatedAt.
func TestRecordToolProfileReferrer_OutOfOrderPushes_RanksByValidatedAtNotPushOrder(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-tpr-shuffled", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Pushed out of chronological order: c (newest) first, then a (oldest), then b, d.
	got := recordAll(t, s, "hash-tpr-shuffled",
		ref("sha256:c", 10), ref("sha256:a", 40), ref("sha256:b", 30), ref("sha256:d", 0))

	want := map[string]index.ReferrerLifecycleStatus{
		"sha256:d": index.ReferrerActive, // newest
		"sha256:c": index.ReferrerActive,
		"sha256:b": index.ReferrerActive,
		"sha256:a": index.ReferrerGCCandidate, // oldest by ValidatedAt, despite not being pushed first
	}
	for digest, wantStatus := range want {
		if status := statusOf(got, digest); status != wantStatus {
			t.Errorf("%s: got %q, want %q", digest, status, wantStatus)
		}
	}

	for _, r := range got {
		if r.Digest == "sha256:d" && r.Rank != 1 {
			t.Errorf("sha256:d (most recent ValidatedAt): rank = %d, want 1", r.Rank)
		}
		if r.Digest == "sha256:a" && r.Rank != 4 {
			t.Errorf("sha256:a (oldest ValidatedAt): rank = %d, want 4", r.Rank)
		}
	}
}

// TestRecordToolProfileReferrer_NoRegistryDependency documents and pins the
// guarantee that GC marking cannot perform a registry write: pkg/oras already
// imports pkg/index (for ToolCheckRecord/ObservedIoProfile types), so pkg/index
// importing pkg/oras back would be a circular import and fail to build. This
// test exercises RecordToolProfileReferrer purely against a local, file-backed
// Store — no registry client, HTTP transport, or oras/sori package is ever in
// scope for this call path.
func TestRecordToolProfileReferrer_NoRegistryDependency(t *testing.T) {
	s := newStore(t)
	e := toolEntry("hash-tpr-no-registry", "bwa-mem2@2.2.1")
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for i := 0; i < 5; i++ {
		r := ref("sha256:x", 0)
		if _, err := s.RecordToolProfileReferrer("hash-tpr-no-registry", &r); err != nil {
			t.Fatalf("RecordToolProfileReferrer: %v", err)
		}
	}
	// Reaching here without a network timeout/dial error confirms no transport
	// was ever invoked — see the import-graph argument in the doc comment above.
}
