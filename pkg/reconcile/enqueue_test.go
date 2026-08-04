// Internal test (package reconcile) so it can inject the EnqueueRetrier's
// unexported now/jitter fields for deterministic backoff/timing assertions.
package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"

	"github.com/HeaInSeo/NodeVault/pkg/index"
)

// fakeEnqueuer records every EnqueueValidationWork request and fails the first
// failFirstN calls (or all calls when alwaysFail is set), so tests can drive a
// transient blip that recovers or a permanent outage that gets abandoned.
type fakeEnqueuer struct {
	reqs       []*nsv1.EnqueueValidationWorkRequest
	failFirstN int
	alwaysFail bool
	jobID      string
}

func (f *fakeEnqueuer) EnqueueValidationWork(
	_ context.Context, req *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	f.reqs = append(f.reqs, req)
	if f.alwaysFail || len(f.reqs) <= f.failFirstN {
		return nil, errors.New("simulated transient enqueue failure")
	}
	job := f.jobID
	if job == "" {
		job = "job-retry"
	}
	return &nsv1.EnqueueValidationWorkResponse{JobId: job, Status: "Queued"}, nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestRetrier builds an EnqueueRetrier over store+enq with a fixed clock and
// zero jitter (jitter()==0.5 => delta 0), so NextAttemptAt is exactly
// now+backoff and assertions are deterministic.
func newTestRetrier(t *testing.T, store *index.Store, enq SentinelEnqueuer, clk *fakeClock, cfg EnqueueRetryConfig) *EnqueueRetrier {
	t.Helper()
	r := NewEnqueueRetrier(store, enq, cfg)
	r.now = clk.now
	r.jitter = func() float64 { return 0.5 }
	return r
}

// seedUnavailable creates a ValidationRequestRecord and drives it to
// ValidationUnavailable exactly as the build path does: created EnqueuePending,
// then EnqueuePending -> Unavailable carrying the failed attempt count. replay
// controls whether the ImageRepository/ToolName replay fields are present.
func seedUnavailable(t *testing.T, store *index.Store, id string, attempts int, nextAt time.Time, replay bool) {
	t.Helper()
	rec := index.ValidationRequestRecord{
		ValidationRequestID: id,
		BuildID:             "build-" + id,
		CasHash:             "cas-" + id,
		ImageDigest:         "sha256:" + id,
	}
	if replay {
		rec.ArtifactKind = "tool"
		rec.ImageRepository = "harbor.example.com/library/" + id
		rec.ToolName = id
		rec.Version = "1.0.0"
		rec.RequestedActions = []string{"smoke_run"}
	}
	if err := store.CreateValidationRequestRecord(rec); err != nil {
		t.Fatalf("CreateValidationRequestRecord: %v", err)
	}
	if err := store.TransitionValidationRequest(id, index.ValidationUnavailable,
		func(rr *index.ValidationRequestRecord) {
			rr.FailureReason = "initial enqueue failed"
			rr.EnqueueAttempts = attempts
			rr.NextAttemptAt = nextAt
		}); err != nil {
		t.Fatalf("seed transition to Unavailable: %v", err)
	}
}

func getRec(t *testing.T, store *index.Store, id string) index.ValidationRequestRecord {
	t.Helper()
	rec, err := store.GetValidationRequestRecord(id)
	if err != nil {
		t.Fatalf("GetValidationRequestRecord(%q): %v", id, err)
	}
	return rec
}

func newStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	return store
}

// TestEnqueueRetrier_RetrySucceeds_ReusesSameRequestID is the core recovery
// path: a request left Unavailable by a transport failure is re-enqueued with
// the SAME ValidationRequestID (never a new one), replaying RequestedActions
// verbatim, and transitions to Queued on success.
func TestEnqueueRetrier_RetrySucceeds_ReusesSameRequestID(t *testing.T) {
	store := newStore(t)
	seedUnavailable(t, store, "vr-1", 1, time.Time{}, true)

	enq := &fakeEnqueuer{jobID: "job-42"}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}

	if len(enq.reqs) != 1 {
		t.Fatalf("enqueue calls = %d, want 1", len(enq.reqs))
	}
	got := enq.reqs[0]
	if got.GetValidationRequestId() != "vr-1" {
		t.Errorf("ValidationRequestId = %q, want vr-1 (same id must be reused)", got.GetValidationRequestId())
	}
	if got.GetCasHash() != "cas-vr-1" || got.GetImageDigest() != "sha256:vr-1" {
		t.Errorf("replayed identity fields wrong: cas=%q digest=%q", got.GetCasHash(), got.GetImageDigest())
	}
	if acts := got.GetRequestedActions(); len(acts) != 1 || acts[0] != "smoke_run" {
		t.Errorf("RequestedActions = %v, want [smoke_run] replayed verbatim", acts)
	}

	rec := getRec(t, store, "vr-1")
	if rec.ValidationStatus != index.ValidationQueued {
		t.Errorf("status = %q, want Queued", rec.ValidationStatus)
	}
	if rec.SentinelJobID != "job-42" {
		t.Errorf("SentinelJobID = %q, want job-42", rec.SentinelJobID)
	}
	if rec.EnqueueAttempts != 2 {
		t.Errorf("EnqueueAttempts = %d, want 2 (initial + this retry)", rec.EnqueueAttempts)
	}
	if !rec.NextAttemptAt.IsZero() {
		t.Errorf("NextAttemptAt = %v, want cleared after success", rec.NextAttemptAt)
	}
}

// TestEnqueueRetrier_RetryFails_ReschedulesWithBackoff verifies a failed retry
// increments the attempt count and pushes NextAttemptAt out by the backoff,
// rather than busy-looping.
func TestEnqueueRetrier_RetryFails_ReschedulesWithBackoff(t *testing.T) {
	store := newStore(t)
	seedUnavailable(t, store, "vr-1", 1, time.Time{}, true)

	enq := &fakeEnqueuer{alwaysFail: true}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	cfg := EnqueueRetryConfig{BaseBackoff: 30 * time.Second, MaxBackoff: 15 * time.Minute}
	r := newTestRetrier(t, store, enq, clk, cfg)

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}

	rec := getRec(t, store, "vr-1")
	if rec.ValidationStatus != index.ValidationUnavailable {
		t.Errorf("status = %q, want Unavailable after a failed retry", rec.ValidationStatus)
	}
	if rec.EnqueueAttempts != 2 {
		t.Errorf("EnqueueAttempts = %d, want 2", rec.EnqueueAttempts)
	}
	// backoff(2) = base*2 = 60s, zero jitter.
	wantNext := clk.t.Add(60 * time.Second)
	if !rec.NextAttemptAt.Equal(wantNext) {
		t.Errorf("NextAttemptAt = %v, want %v (now + backoff(2))", rec.NextAttemptAt, wantNext)
	}
	if rec.FailureReason == "" {
		t.Error("FailureReason empty, want the enqueue error")
	}
}

// TestEnqueueRetrier_RespectsNextAttemptAt proves the loop does not retry a
// record still inside its backoff window (no busy loop).
func TestEnqueueRetrier_RespectsNextAttemptAt(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	future := clk.t.Add(10 * time.Minute)
	seedUnavailable(t, store, "vr-1", 2, future, true)

	enq := &fakeEnqueuer{alwaysFail: true}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("enqueue calls = %d, want 0 (still backing off)", len(enq.reqs))
	}
	rec := getRec(t, store, "vr-1")
	if rec.ValidationStatus != index.ValidationUnavailable || rec.EnqueueAttempts != 2 {
		t.Errorf("record changed while backing off: status=%q attempts=%d", rec.ValidationStatus, rec.EnqueueAttempts)
	}
}

// TestEnqueueRetrier_EscalatesToAbandonedAtMaxAttempts verifies terminal
// escalation once the budget is spent, and that escalation ignores an unelapsed
// NextAttemptAt (a request that will never be retried should not wait it out).
func TestEnqueueRetrier_EscalatesToAbandonedAtMaxAttempts(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	future := clk.t.Add(1 * time.Hour)
	seedUnavailable(t, store, "vr-1", 5, future, true) // attempts == default MaxAttempts

	enq := &fakeEnqueuer{alwaysFail: true}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("enqueue calls = %d, want 0 (budget exhausted, must not retry)", len(enq.reqs))
	}
	rec := getRec(t, store, "vr-1")
	if rec.ValidationStatus != index.ValidationEnqueueAbandoned {
		t.Fatalf("status = %q, want EnqueueAbandoned", rec.ValidationStatus)
	}

	// Abandoned is terminal: no outgoing edge, so a further transition is rejected.
	err := store.TransitionValidationRequest("vr-1", index.ValidationEnqueuePending, nil)
	if !errors.Is(err, index.ErrInvalidTransition) {
		t.Errorf("transition out of Abandoned err = %v, want ErrInvalidTransition (terminal)", err)
	}
}

// TestEnqueueRetrier_SkipsPreFeatureRecordMissingReplayFields verifies a record
// written before the replay fields existed (no ImageRepository/ToolName) is
// left completely untouched — never retried, never escalated (no backfill).
func TestEnqueueRetrier_SkipsPreFeatureRecordMissingReplayFields(t *testing.T) {
	store := newStore(t)
	seedUnavailable(t, store, "vr-old", 1, time.Time{}, false) // replay=false

	enq := &fakeEnqueuer{alwaysFail: true}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("enqueue calls = %d, want 0 (cannot rebuild request)", len(enq.reqs))
	}
	rec := getRec(t, store, "vr-old")
	if rec.ValidationStatus != index.ValidationUnavailable || rec.EnqueueAttempts != 1 {
		t.Errorf("pre-feature record was modified: status=%q attempts=%d", rec.ValidationStatus, rec.EnqueueAttempts)
	}
}

// TestEnqueueRetrier_ExhaustsBudgetThenAbandons is the end-to-end guard across
// ticks: a permanently-down NodeSentinel is retried up to the budget (reusing
// the same id every time, honoring NextAttemptAt between ticks) and then
// abandoned — never an unbounded loop.
func TestEnqueueRetrier_ExhaustsBudgetThenAbandons(t *testing.T) {
	store := newStore(t)
	seedUnavailable(t, store, "vr-1", 1, time.Time{}, true) // 1 attempt already made (build path)

	enq := &fakeEnqueuer{alwaysFail: true}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	cfg := EnqueueRetryConfig{MaxAttempts: 5}
	r := newTestRetrier(t, store, enq, clk, cfg)

	// Drive ticks until the record leaves Unavailable, advancing the clock past
	// each scheduled NextAttemptAt so backoff never blocks a due retry. The
	// loop bound is a safety net: correct behavior terminates well before it.
	for i := 0; i < 20; i++ {
		if err := r.RetryDue(context.Background()); err != nil {
			t.Fatalf("RetryDue tick %d: %v", i, err)
		}
		rec := getRec(t, store, "vr-1")
		if rec.ValidationStatus != index.ValidationUnavailable {
			break
		}
		next := rec.NextAttemptAt
		if next.IsZero() || !next.After(clk.t) {
			clk.advance(time.Second) // eligible now; nudge forward to avoid a tight spin
		} else {
			clk.t = next.Add(time.Second)
		}
	}

	rec := getRec(t, store, "vr-1")
	if rec.ValidationStatus != index.ValidationEnqueueAbandoned {
		t.Fatalf("final status = %q, want EnqueueAbandoned", rec.ValidationStatus)
	}
	// attempts 2,3,4,5 are the reconciler retries (attempt 1 was the build
	// path, not counted here); attempt 5 hits the budget, next tick abandons.
	if len(enq.reqs) != 4 {
		t.Errorf("total retry enqueue calls = %d, want 4 (attempts 2..5)", len(enq.reqs))
	}
	for i, req := range enq.reqs {
		if req.GetValidationRequestId() != "vr-1" {
			t.Errorf("retry %d used ValidationRequestId %q, want vr-1 (id must be reused)", i, req.GetValidationRequestId())
		}
	}
	if rec.EnqueueAttempts != 5 {
		t.Errorf("EnqueueAttempts = %d, want 5 at abandonment", rec.EnqueueAttempts)
	}
}
