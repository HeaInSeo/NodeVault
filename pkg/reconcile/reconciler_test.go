package reconcile_test

import (
	"context"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/reconcile"
)

// fakeChecker implements RegistryChecker for tests.
type fakeChecker struct {
	imageExists    bool
	referrerExists bool
	pullReachable  bool
	imageErr       error
	referrerErr    error
	pullErr        error
}

func (f *fakeChecker) ImageExists(_ context.Context, _, _ string) (bool, error) {
	return f.imageExists, f.imageErr
}
func (f *fakeChecker) ReferrerExists(_ context.Context, _, _ string) (bool, error) {
	return f.referrerExists, f.referrerErr
}
func (f *fakeChecker) PullReachable(_ context.Context, _, _ string) (bool, error) {
	return f.pullReachable, f.pullErr
}

func newTestStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return store
}

func appendEntry(t *testing.T, store *index.Store, casHash, stableRef, imageDigest string) {
	t.Helper()
	err := store.Append(index.Entry{
		CasHash:         casHash,
		ArtifactKind:    index.KindTool,
		StableRef:       stableRef,
		ImageDigest:     imageDigest,
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// ── judgeHealth via FastRun ───────────────────────────────────────────────────

func TestFastRun_ImageAndReferrer_Healthy(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")

	r := reconcile.New(store, &fakeChecker{imageExists: true, referrerExists: true})
	if err := r.FastRun(t.Context()); err != nil {
		t.Fatalf("FastRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthHealthy {
		t.Errorf("want Healthy, got %q", e.IntegrityHealth)
	}
}

func TestFastRun_ImageOK_ReferrerMissing_Partial(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")

	r := reconcile.New(store, &fakeChecker{imageExists: true, referrerExists: false})
	if err := r.FastRun(t.Context()); err != nil {
		t.Fatalf("FastRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthPartial {
		t.Errorf("want Partial, got %q", e.IntegrityHealth)
	}
}

func TestFastRun_BothMissing_Missing(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")

	r := reconcile.New(store, &fakeChecker{imageExists: false, referrerExists: false})
	if err := r.FastRun(t.Context()); err != nil {
		t.Fatalf("FastRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthMissing {
		t.Errorf("want Missing, got %q", e.IntegrityHealth)
	}
}

func TestFastRun_ImageMissing_ReferrerPresent_Orphaned(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")

	r := reconcile.New(store, &fakeChecker{imageExists: false, referrerExists: true})
	if err := r.FastRun(t.Context()); err != nil {
		t.Fatalf("FastRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthOrphaned {
		t.Errorf("want Orphaned, got %q", e.IntegrityHealth)
	}
}

// ── SlowRun ───────────────────────────────────────────────────────────────────

func TestSlowRun_PullFail_Unreachable(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	// Entry starts Healthy (appendEntry sets it).

	r := reconcile.New(store, &fakeChecker{
		imageExists: true, referrerExists: true, pullReachable: false,
	})
	if err := r.SlowRun(t.Context()); err != nil {
		t.Fatalf("SlowRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthUnreachable {
		t.Errorf("want Unreachable, got %q", e.IntegrityHealth)
	}
}

func TestSlowRun_PullOK_HealthUnchanged(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")

	r := reconcile.New(store, &fakeChecker{
		imageExists: true, referrerExists: true, pullReachable: true,
	})
	if err := r.SlowRun(t.Context()); err != nil {
		t.Fatalf("SlowRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthHealthy {
		t.Errorf("want Healthy, got %q", e.IntegrityHealth)
	}
}

// raceChecker simulates a FastRun pass landing a fresher integrity_health write
// while SlowRun's slow PullReachable check for the same entry is still in flight —
// reproducing the stale-snapshot race deterministically (no timing dependency).
type raceChecker struct {
	store            *index.Store
	casHash          string
	concurrentHealth index.IntegrityHealth // health FastRun "lands" mid-pull
	pullReachable    bool
}

func (*raceChecker) ImageExists(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (*raceChecker) ReferrerExists(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (c *raceChecker) PullReachable(_ context.Context, _, _ string) (bool, error) {
	// Simulate a concurrent FastRun tick completing while this slow pull check is
	// still in flight, transitioning the entry away from the Healthy snapshot
	// SlowRun read before starting this call.
	if err := c.store.SetIntegrityHealth(c.casHash, c.concurrentHealth); err != nil {
		return false, err
	}
	return c.pullReachable, nil
}

func TestSlowRun_ConcurrentFastRunChange_NotClobbered(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	// Entry starts Healthy (appendEntry sets it) — SlowRun will snapshot this value.

	r := reconcile.New(store, &raceChecker{
		store:            store,
		casHash:          "h1",
		concurrentHealth: index.HealthPartial, // fresher verdict landed by "FastRun"
		pullReachable:    false,               // slow pull check fails
	})
	if err := r.SlowRun(t.Context()); err != nil {
		t.Fatalf("SlowRun: %v", err)
	}

	// The fresher Partial verdict (landed mid-flight) must win — SlowRun's stale
	// Unreachable write, based on the pre-interleaving Healthy snapshot, must be
	// discarded rather than clobbering it.
	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthPartial {
		t.Errorf("want Partial (fresher FastRun verdict preserved), got %q", e.IntegrityHealth)
	}
}

func TestSlowRun_SkipsNonHealthy(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	// Manually set to Partial (non-Healthy) before slow run.
	if err := store.SetIntegrityHealth("h1", index.HealthPartial); err != nil {
		t.Fatalf("SetIntegrityHealth: %v", err)
	}

	// pullReachable=false — but slow run must skip Partial entries.
	r := reconcile.New(store, &fakeChecker{pullReachable: false})
	if err := r.SlowRun(t.Context()); err != nil {
		t.Fatalf("SlowRun: %v", err)
	}

	// Health must remain Partial (slow loop skipped it).
	e, _ := store.GetByCasHash("h1")
	if e.IntegrityHealth != index.HealthPartial {
		t.Errorf("SlowRun must not change non-Healthy entries; got %q", e.IntegrityHealth)
	}
}

// ── ReconcileOne ─────────────────────────────────────────────────────────────

func TestReconcileOne_TargetedUpdate(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	appendEntry(t, store, "h2", "samtools@1.17", "sha256:bbb")

	// Only h1 will trigger reconcile; checker sees image missing.
	r := reconcile.New(store, &fakeChecker{imageExists: false, referrerExists: false})
	if err := r.ReconcileOne(t.Context(), "h1"); err != nil {
		t.Fatalf("ReconcileOne: %v", err)
	}

	e1, _ := store.GetByCasHash("h1")
	e2, _ := store.GetByCasHash("h2")

	if e1.IntegrityHealth != index.HealthMissing {
		t.Errorf("h1: want Missing, got %q", e1.IntegrityHealth)
	}
	// h2 must be untouched.
	if e2.IntegrityHealth != index.HealthHealthy {
		t.Errorf("h2 must remain Healthy (not targeted), got %q", e2.IntegrityHealth)
	}
}

// ── Axis independence: reconcile NEVER changes lifecycle_phase ────────────────

func TestFastRun_NeverChangesLifecyclePhase(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	// Manually retract to simulate operator intent.
	if err := store.SetLifecyclePhase("h1", index.PhaseRetracted); err != nil {
		t.Fatalf("SetLifecyclePhase: %v", err)
	}

	r := reconcile.New(store, &fakeChecker{imageExists: false, referrerExists: false})
	if err := r.FastRun(t.Context()); err != nil {
		t.Fatalf("FastRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.LifecyclePhase != index.PhaseRetracted {
		t.Errorf("FastRun must NOT change lifecycle_phase; got %q", e.LifecyclePhase)
	}
}

func TestSlowRun_NeverChangesLifecyclePhase(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	if err := store.SetLifecyclePhase("h1", index.PhaseRetracted); err != nil {
		t.Fatalf("SetLifecyclePhase: %v", err)
	}
	// Reset to Healthy so slow loop would process it if it didn't check lifecycle.
	if err := store.SetIntegrityHealth("h1", index.HealthHealthy); err != nil {
		t.Fatalf("SetIntegrityHealth: %v", err)
	}

	r := reconcile.New(store, &fakeChecker{pullReachable: false})
	if err := r.SlowRun(t.Context()); err != nil {
		t.Fatalf("SlowRun: %v", err)
	}

	e, _ := store.GetByCasHash("h1")
	if e.LifecyclePhase != index.PhaseRetracted {
		t.Errorf("SlowRun must NOT change lifecycle_phase; got %q", e.LifecyclePhase)
	}
}

// ── Multiple artifacts ────────────────────────────────────────────────────────

func TestFastRun_MultipleArtifacts_EachUpdated(t *testing.T) {
	store := newTestStore(t)
	appendEntry(t, store, "h1", "bwa@0.7.17", "sha256:aaa")
	appendEntry(t, store, "h2", "samtools@1.17", "sha256:bbb")

	// Checker always returns image ok, referrer missing → all Partial.
	r := reconcile.New(store, &fakeChecker{imageExists: true, referrerExists: false})
	if err := r.FastRun(t.Context()); err != nil {
		t.Fatalf("FastRun: %v", err)
	}

	for _, hash := range []string{"h1", "h2"} {
		e, _ := store.GetByCasHash(hash)
		if e.IntegrityHealth != index.HealthPartial {
			t.Errorf("%s: want Partial, got %q", hash, e.IntegrityHealth)
		}
	}
}
