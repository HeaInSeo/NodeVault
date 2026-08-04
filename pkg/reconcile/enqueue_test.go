// Internal test (package reconcile) so it can inject the EnqueueRetrier's
// unexported now/jitter fields for deterministic backoff/timing assertions.
package reconcile

import (
	"context"
	"errors"
	"fmt"
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
	// onCall, if set, runs at the start of each EnqueueValidationWork — used to
	// observe the durable record's status at the exact moment of the RPC.
	onCall func(*nsv1.EnqueueValidationWorkRequest)
}

func (f *fakeEnqueuer) EnqueueValidationWork(
	_ context.Context, req *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	if f.onCall != nil {
		f.onCall(req)
	}
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

// seedPending creates a ValidationRequestRecord left in EnqueuePending exactly as
// the build path leaves it before it calls NodeSentinel, with an explicit
// RequestedAt so tests control its age against the fake clock. replay controls
// whether the ImageRepository/ToolName replay fields are present. EnqueueAttempts
// is left 0, matching a record that crashed before any enqueue outcome.
func seedPending(t *testing.T, store *index.Store, id string, requestedAt time.Time, replay bool) {
	t.Helper()
	rec := index.ValidationRequestRecord{
		ValidationRequestID: id,
		BuildID:             "build-" + id,
		CasHash:             "cas-" + id,
		ImageDigest:         "sha256:" + id,
		RequestedAt:         requestedAt,
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
	if got := getRec(t, store, id).ValidationStatus; got != index.ValidationEnqueuePending {
		t.Fatalf("seed status = %q, want EnqueuePending", got)
	}
}

// idempotentSentinel models NodeSentinel's same-id idempotency contract: an
// EnqueueValidationWork for a ValidationRequestId already seen returns the same
// JobId and creates no new logical job. NodeVault's recovery re-send depends on
// that contract holding on the NodeSentinel side; the enforcement itself lives in
// the NodeSentinel repo (not modified here). This fake locks NodeVault's half of
// the contract — it must always re-send the exact same ValidationRequestId so the
// dedup key is stable.
type idempotentSentinel struct {
	jobs  map[string]string // validation_request_id -> job_id
	calls int
	seq   int
}

func (s *idempotentSentinel) EnqueueValidationWork(
	_ context.Context, req *nsv1.EnqueueValidationWorkRequest,
) (*nsv1.EnqueueValidationWorkResponse, error) {
	s.calls++
	if s.jobs == nil {
		s.jobs = map[string]string{}
	}
	id := req.GetValidationRequestId()
	if job, ok := s.jobs[id]; ok {
		return &nsv1.EnqueueValidationWorkResponse{JobId: job, Status: "Queued"}, nil
	}
	s.seq++
	job := fmt.Sprintf("job-%d", s.seq)
	s.jobs[id] = job
	return &nsv1.EnqueueValidationWorkResponse{JobId: job, Status: "Queued"}, nil
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

// TestEnqueueRetryConfig_DefaultsApplyJitter is the P2 guard: a default
// EnqueueRetryConfig{} must get the default jitter fraction, not zero —
// otherwise a backlog that built up during an outage retries as a synchronized
// cohort. Before the fix withDefaults left a zero JitterFrac untouched.
func TestEnqueueRetryConfig_DefaultsApplyJitter(t *testing.T) {
	got := EnqueueRetryConfig{}.withDefaults()
	if got.JitterFrac != defaultEnqueueJitterFrac {
		t.Errorf("default JitterFrac = %v, want %v (zero-value config must apply default jitter)",
			got.JitterFrac, defaultEnqueueJitterFrac)
	}
	if got.JitterFrac <= 0 {
		t.Errorf("default JitterFrac = %v, want > 0 to desync retry cohorts", got.JitterFrac)
	}
	// An explicit in-range value is preserved.
	if v := (EnqueueRetryConfig{JitterFrac: 0.5}).withDefaults().JitterFrac; v != 0.5 {
		t.Errorf("explicit JitterFrac = %v, want 0.5 preserved", v)
	}
}

// TestEnqueueRetrier_RetryDoesNotStrandEnqueuePending is the P1 guard: the retry
// path must never persist a bare EnqueuePending status. The record stays
// Unavailable across the RPC, so a crash mid-retry leaves it recoverable by the
// next RetryDue (which only scans Unavailable). We observe the durable status
// at the exact moment of the enqueue call, and after a failed retry. Before the
// fix, retryOne persisted Unavailable -> EnqueuePending before the RPC, so the
// observed status would be EnqueuePending and the failed record would be
// stranded there.
func TestEnqueueRetrier_RetryDoesNotStrandEnqueuePending(t *testing.T) {
	store := newStore(t)
	seedUnavailable(t, store, "vr-1", 1, time.Time{}, true)

	enq := &fakeEnqueuer{alwaysFail: true}
	var observed []index.ValidationStatus
	enq.onCall = func(*nsv1.EnqueueValidationWorkRequest) {
		observed = append(observed, getRec(t, store, "vr-1").ValidationStatus)
	}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("enqueue observations = %d, want 1", len(observed))
	}
	if observed[0] != index.ValidationUnavailable {
		t.Errorf("status during RPC = %q, want Unavailable (must not persist a bare EnqueuePending)", observed[0])
	}
	// After a failed retry the record is still Unavailable, never EnqueuePending.
	if got := getRec(t, store, "vr-1").ValidationStatus; got != index.ValidationUnavailable {
		t.Errorf("status after failed retry = %q, want Unavailable", got)
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

// TestEnqueueRetrier_RecoversStrandedEnqueuePending is the outbox guard: a record
// the build path left in EnqueuePending (persisted before its enqueue, then
// crashed before any outcome) is recovered once it is older than
// PendingStaleAfter. The loop converts it to Unavailable and, on the next tick,
// re-sends it with the SAME ValidationRequestID, driving it to Queued. Before this
// change RetryDue only scanned Unavailable, so the record stayed stranded forever.
func TestEnqueueRetrier_RecoversStrandedEnqueuePending(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	// Stranded 60s ago; default PendingStaleAfter is 30s, so it is stale.
	seedPending(t, store, "vp-1", clk.t.Add(-60*time.Second), true)

	enq := &fakeEnqueuer{jobID: "job-42"}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	// Tick 1 recovers the stranded record to Unavailable (no re-send yet).
	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue tick 1: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("enqueue calls after recovery tick = %d, want 0 (re-send happens next tick)", len(enq.reqs))
	}
	rec := getRec(t, store, "vp-1")
	if rec.ValidationStatus != index.ValidationUnavailable {
		t.Fatalf("status after recovery = %q, want Unavailable", rec.ValidationStatus)
	}
	if rec.EnqueueAttempts != 0 {
		t.Errorf("EnqueueAttempts = %d, want 0 preserved (full budget kept)", rec.EnqueueAttempts)
	}
	if rec.FailureReason == "" {
		t.Error("FailureReason empty, want a stranded-recovery explanation")
	}

	// Tick 2 re-sends via the existing Unavailable path, reusing the same id.
	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue tick 2: %v", err)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("enqueue calls = %d, want 1", len(enq.reqs))
	}
	if got := enq.reqs[0].GetValidationRequestId(); got != "vp-1" {
		t.Errorf("re-sent ValidationRequestId = %q, want vp-1 (same id must be reused)", got)
	}
	if acts := enq.reqs[0].GetRequestedActions(); len(acts) != 1 || acts[0] != "smoke_run" {
		t.Errorf("RequestedActions = %v, want [smoke_run] replayed verbatim (no added action)", acts)
	}
	rec = getRec(t, store, "vp-1")
	if rec.ValidationStatus != index.ValidationQueued {
		t.Errorf("status after re-send = %q, want Queued", rec.ValidationStatus)
	}
	if rec.SentinelJobID != "job-42" {
		t.Errorf("SentinelJobID = %q, want job-42", rec.SentinelJobID)
	}
}

// TestEnqueueRetrier_ResultCorrelatesAfterRecoveryBeforeReenqueue covers the
// window codex P1 flagged: a stranded request whose original enqueue actually
// reached NodeSentinel is recovered to Unavailable, and before the next re-send
// tick a terminal result for the already-running job arrives. It must correlate
// (Unavailable -> Running -> terminal) rather than be swallowed, and a later
// RetryDue tick must then leave the now-terminal record alone (no re-send, no
// corruption).
func TestEnqueueRetrier_ResultCorrelatesAfterRecoveryBeforeReenqueue(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	seedPending(t, store, "vp-1", clk.t.Add(-2*time.Minute), true) // stranded, stale

	enq := &fakeEnqueuer{alwaysFail: true} // a re-send, if attempted, would fail
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	// Tick 1: recover the stranded record to Unavailable (no re-send yet).
	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue tick 1: %v", err)
	}
	if got := getRec(t, store, "vp-1").ValidationStatus; got != index.ValidationUnavailable {
		t.Fatalf("status after recovery = %q, want Unavailable", got)
	}

	// A terminal result for the already-running job arrives while Unavailable.
	if err := store.AppendToolCheckRecordCorrelated(
		index.ToolCheckRecord{
			CheckID: "chk-1", ImageDigest: "sha256:vp-1", ValidationStatus: "succeeded",
			Terminal: true, ValidationRequestID: "vp-1", SentinelJobID: "job-1",
		},
		"vp-1", "job-1", true, true, "",
	); err != nil {
		t.Fatalf("AppendToolCheckRecordCorrelated: %v", err)
	}
	if got := getRec(t, store, "vp-1").ValidationStatus; got != index.ValidationSucceeded {
		t.Fatalf("status after result = %q, want Succeeded (must correlate, not be swallowed)", got)
	}

	// Tick 2: the record is terminal now (not Unavailable), so nothing is re-sent
	// and the terminal state is preserved.
	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue tick 2: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Errorf("enqueue calls = %d, want 0 (terminal record must not be re-sent)", len(enq.reqs))
	}
	if got := getRec(t, store, "vp-1").ValidationStatus; got != index.ValidationSucceeded {
		t.Errorf("status = %q, want Succeeded preserved", got)
	}
}

// TestEnqueueRetrier_LeavesFreshEnqueuePending proves a record still inside the
// PendingStaleAfter window is treated as possibly-in-flight and left completely
// untouched — never converted, never re-sent.
func TestEnqueueRetrier_LeavesFreshEnqueuePending(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	// Only 10s old; default PendingStaleAfter is 30s, so it is still in-flight.
	seedPending(t, store, "vr-fresh", clk.t.Add(-10*time.Second), true)

	enq := &fakeEnqueuer{alwaysFail: true}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("enqueue calls = %d, want 0 (fresh pending is not stranded)", len(enq.reqs))
	}
	rec := getRec(t, store, "vr-fresh")
	if rec.ValidationStatus != index.ValidationEnqueuePending {
		t.Errorf("status = %q, want EnqueuePending left untouched", rec.ValidationStatus)
	}
	if rec.FailureReason != "" {
		t.Errorf("FailureReason = %q, want empty (record must not be modified)", rec.FailureReason)
	}
}

// TestEnqueueRetrier_SkipsStrandedPendingMissingReplayFields verifies a stale
// EnqueuePending record without the replay fields (pre-feature) is left untouched:
// it cannot be re-sent, so it is neither converted nor backfilled.
func TestEnqueueRetrier_SkipsStrandedPendingMissingReplayFields(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	seedPending(t, store, "vr-old", clk.t.Add(-10*time.Minute), false) // replay=false, well past stale

	enq := &fakeEnqueuer{alwaysFail: true}
	r := newTestRetrier(t, store, enq, clk, EnqueueRetryConfig{})

	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("enqueue calls = %d, want 0 (cannot rebuild request)", len(enq.reqs))
	}
	rec := getRec(t, store, "vr-old")
	if rec.ValidationStatus != index.ValidationEnqueuePending {
		t.Errorf("status = %q, want EnqueuePending left untouched (no backfill)", rec.ValidationStatus)
	}
}

// TestEnqueueRetryConfig_PendingStaleAfterExceedsTimeout locks the config
// invariant: PendingStaleAfter takes a default and, however a caller sets it,
// always ends strictly greater than EnqueueTimeout so an in-flight enqueue is
// never misjudged stranded.
func TestEnqueueRetryConfig_PendingStaleAfterExceedsTimeout(t *testing.T) {
	// Zero-value config: default applied and it exceeds the (defaulted) timeout.
	got := EnqueueRetryConfig{}.withDefaults()
	if got.PendingStaleAfter != defaultPendingStaleAfter {
		t.Errorf("default PendingStaleAfter = %v, want %v", got.PendingStaleAfter, defaultPendingStaleAfter)
	}
	if got.PendingStaleAfter <= got.EnqueueTimeout {
		t.Errorf("PendingStaleAfter %v must exceed EnqueueTimeout %v", got.PendingStaleAfter, got.EnqueueTimeout)
	}

	// Too-low value (<= EnqueueTimeout) is raised above the timeout.
	tooLow := EnqueueRetryConfig{EnqueueTimeout: 60 * time.Second, PendingStaleAfter: 20 * time.Second}.withDefaults()
	if tooLow.PendingStaleAfter <= tooLow.EnqueueTimeout {
		t.Errorf("PendingStaleAfter %v was not raised above EnqueueTimeout %v",
			tooLow.PendingStaleAfter, tooLow.EnqueueTimeout)
	}

	// Equal value is also rejected (must be strictly greater).
	eq := EnqueueRetryConfig{EnqueueTimeout: 30 * time.Second, PendingStaleAfter: 30 * time.Second}.withDefaults()
	if eq.PendingStaleAfter <= eq.EnqueueTimeout {
		t.Errorf("equal PendingStaleAfter %v was not raised above EnqueueTimeout %v",
			eq.PendingStaleAfter, eq.EnqueueTimeout)
	}

	// An explicit safe value (> timeout) is preserved untouched.
	if v := (EnqueueRetryConfig{EnqueueTimeout: 5 * time.Second, PendingStaleAfter: 45 * time.Second}).
		withDefaults().PendingStaleAfter; v != 45*time.Second {
		t.Errorf("explicit PendingStaleAfter = %v, want 45s preserved", v)
	}
}

// TestEnqueueRetrier_SameIDReenqueueIsIdempotent locks NodeVault's half of the
// NodeSentinel same-id idempotency contract. A stranded EnqueuePending whose
// original enqueue actually reached NodeSentinel before the crash is recovered and
// re-sent; because the re-send reuses the same ValidationRequestID, an idempotent
// NodeSentinel returns the ORIGINAL job and creates no duplicate. The record ends
// bound to that original job, not a second one. (NodeSentinel's own dedup
// enforcement lives in the NodeSentinel repo and is not modified here.)
func TestEnqueueRetrier_SameIDReenqueueIsIdempotent(t *testing.T) {
	store := newStore(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	seedPending(t, store, "vr-1", clk.t.Add(-2*time.Minute), true) // stale

	// Model the ambiguous crash: the original enqueue already registered job-1 on
	// NodeSentinel for vr-1 before the process died persisting the outcome.
	sent := &idempotentSentinel{jobs: map[string]string{"vr-1": "job-1"}, seq: 1}
	r := newTestRetrier(t, store, sent, clk, EnqueueRetryConfig{})

	// Tick 1 recovers to Unavailable; tick 2 re-sends the same id.
	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue tick 1: %v", err)
	}
	if err := r.RetryDue(context.Background()); err != nil {
		t.Fatalf("RetryDue tick 2: %v", err)
	}

	if len(sent.jobs) != 1 {
		t.Errorf("distinct sentinel jobs = %d, want 1 (same-id re-send must not create a new job)", len(sent.jobs))
	}
	rec := getRec(t, store, "vr-1")
	if rec.ValidationStatus != index.ValidationQueued {
		t.Errorf("status = %q, want Queued", rec.ValidationStatus)
	}
	if rec.SentinelJobID != "job-1" {
		t.Errorf("SentinelJobID = %q, want job-1 (the original job, not a duplicate)", rec.SentinelJobID)
	}
}
