package reconcile

import (
	"context"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
)

// panicChecker panics on every call, simulating an unexpected bug in the
// registry client that FastRun/SlowRun depend on.
type panicChecker struct{}

func (panicChecker) ImageExists(context.Context, string, string) (bool, error) {
	panic("simulated unexpected checker failure")
}
func (panicChecker) ReferrerExists(context.Context, string, string) (bool, error) {
	panic("simulated unexpected checker failure")
}
func (panicChecker) PullReachable(context.Context, string, string) (bool, error) {
	panic("simulated unexpected checker failure")
}

func newPanicTestStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	if err := store.Append(index.Entry{
		CasHash:         "panic-h1",
		ArtifactKind:    index.KindTool,
		StableRef:       "bwa@0.7.17",
		ImageDigest:     "sha256:aaa",
		LifecyclePhase:  index.PhaseActive,
		IntegrityHealth: index.HealthHealthy,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return store
}

// TestRunFastTick_PanicRecovered verifies a panic inside FastRun is recovered
// rather than left to crash the whole process — runFastTick runs inside its
// own ticker-driven goroutine (see RunFastLoop) with nothing else able to
// catch a panic there. Reaching this test's assertions at all (rather than
// crashing the `go test` binary) is itself part of what is being verified.
func TestRunFastTick_PanicRecovered(t *testing.T) {
	before := metrics.ReconcileErrorTotal.Value()
	r := New(newPanicTestStore(t), panicChecker{})

	r.runFastTick(t.Context())

	if got := metrics.ReconcileErrorTotal.Value(); got != before+1 {
		t.Errorf("ReconcileErrorTotal = %d, want %d (recovered panic counted as an error)", got, before+1)
	}
}

// TestRunSlowTick_PanicRecovered mirrors TestRunFastTick_PanicRecovered for
// the slow loop's PullReachable path.
func TestRunSlowTick_PanicRecovered(t *testing.T) {
	before := metrics.ReconcileErrorTotal.Value()
	r := New(newPanicTestStore(t), panicChecker{})

	r.runSlowTick(t.Context())

	if got := metrics.ReconcileErrorTotal.Value(); got != before+1 {
		t.Errorf("ReconcileErrorTotal = %d, want %d (recovered panic counted as an error)", got, before+1)
	}
}
