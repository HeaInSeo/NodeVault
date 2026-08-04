// Enqueue-retry loop.
//
// EnqueueRetrier recovers NodeSentinel validation enqueues that failed in
// transport/process before the work was ever queued. pkg/build marks such a
// request ValidationUnavailable (EnqueueAttempts=1, NextAttemptAt zero) and
// does not retry it; this loop owns every retry from there on.
//
// Contract:
//   - Reuse the same ValidationRequestID — a retry is the same logical request,
//     never a new one (see index.ValidationRequestRecord's doc comment).
//   - Replay the original EnqueueValidationWorkRequest verbatim from the record;
//     add nothing (in particular no extra RequestedActions).
//   - Bound retries: exponential backoff with jitter, a max attempt budget, a
//     NextAttemptAt gate, and terminal escalation to ValidationEnqueueAbandoned
//     once the budget is spent — never a busy loop.
//   - Touch only the reconcile/enqueue axis. Like the rest of this package it
//     never calls SetLifecyclePhase.

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"runtime/debug"
	"time"

	nsv1 "github.com/HeaInSeo/NodeVault/protos/nodesentinel/v1"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/metrics"
)

// SentinelEnqueuer enqueues validation work with NodeSentinel. It is the same
// shape as pkg/build.SentinelEnqueuer, redeclared here so this package depends
// only on pkg/index and the NodeSentinel proto, not on pkg/build.
type SentinelEnqueuer interface {
	EnqueueValidationWork(
		ctx context.Context, req *nsv1.EnqueueValidationWorkRequest,
	) (*nsv1.EnqueueValidationWorkResponse, error)
}

// EnqueueRetryConfig tunes the enqueue-retry loop. Zero fields take the package
// defaults (see withDefaults). These are operational policy knobs, not values
// derived from any observed record.
type EnqueueRetryConfig struct {
	// MaxAttempts is the total enqueue attempt budget per logical request,
	// counting the initial build-path attempt. Once EnqueueAttempts reaches it
	// the request is escalated to ValidationEnqueueAbandoned instead of retried.
	MaxAttempts int
	// BaseBackoff is the delay after the first failed attempt; it doubles each
	// further attempt up to MaxBackoff.
	BaseBackoff time.Duration
	// MaxBackoff caps the per-attempt backoff.
	MaxBackoff time.Duration
	// JitterFrac spreads each backoff by ±(JitterFrac * backoff), in [0,1].
	JitterFrac float64
	// EnqueueTimeout bounds a single EnqueueValidationWork call.
	EnqueueTimeout time.Duration
}

const (
	defaultMaxEnqueueAttempts = 5
	defaultBaseEnqueueBackoff = 30 * time.Second
	defaultMaxEnqueueBackoff  = 15 * time.Minute
	defaultEnqueueJitterFrac  = 0.2
	defaultEnqueueTimeout     = 5 * time.Second
)

func (c EnqueueRetryConfig) withDefaults() EnqueueRetryConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxEnqueueAttempts
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = defaultBaseEnqueueBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultMaxEnqueueBackoff
	}
	if c.MaxBackoff < c.BaseBackoff {
		c.MaxBackoff = c.BaseBackoff
	}
	if c.JitterFrac < 0 || c.JitterFrac > 1 {
		c.JitterFrac = defaultEnqueueJitterFrac
	}
	if c.EnqueueTimeout <= 0 {
		c.EnqueueTimeout = defaultEnqueueTimeout
	}
	return c
}

// EnqueueRetrier retries transport/process-failed NodeSentinel enqueues.
// Its RetryDue pass is meant to run from a single loop goroutine (RunLoop);
// its now/jitter sources are not safe for concurrent use.
type EnqueueRetrier struct {
	store    *index.Store
	sentinel SentinelEnqueuer
	cfg      EnqueueRetryConfig
	now      func() time.Time
	jitter   func() float64 // returns a value in [0,1)
}

// NewEnqueueRetrier builds an EnqueueRetrier. store and sentinel must be
// non-nil; a nil sentinel means NodeSentinel is disabled and there is nothing
// to retry, so callers should not start the loop in that case.
func NewEnqueueRetrier(store *index.Store, sentinel SentinelEnqueuer, cfg EnqueueRetryConfig) *EnqueueRetrier {
	//nolint:gosec // jitter is non-cryptographic scheduling noise, not a security primitive.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &EnqueueRetrier{
		store:    store,
		sentinel: sentinel,
		cfg:      cfg.withDefaults(),
		now:      func() time.Time { return time.Now().UTC() },
		jitter:   rng.Float64,
	}
}

// RunLoop starts a background goroutine that runs one RetryDue pass every
// interval until ctx is canceled. Mirrors the fast/slow reconcile loops:
// single detached goroutine, panic-recovered so one bad pass cannot crash the
// single-replica process.
func (r *EnqueueRetrier) RunLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runTick(ctx)
			}
		}
	}()
}

func (r *EnqueueRetrier) runTick(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("reconcile: enqueue-retry loop panic recovered",
				"panic", rec, "stack", string(debug.Stack()))
			metrics.ReconcileErrorTotal.Add(1)
		}
	}()
	if err := r.RetryDue(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("reconcile: enqueue-retry pass error", "err", err)
		metrics.ReconcileErrorTotal.Add(1)
	}
}

// RetryDue runs one pass: for every request stuck in ValidationUnavailable it
// escalates an exhausted request to ValidationEnqueueAbandoned, skips one still
// inside its backoff window or missing the replay fields, and otherwise retries
// the enqueue reusing the same ValidationRequestID.
func (r *EnqueueRetrier) RetryDue(ctx context.Context) error {
	recs, err := r.store.ListValidationRequestsByStatus(index.ValidationUnavailable)
	if err != nil {
		return fmt.Errorf("enqueue retry: list unavailable: %w", err)
	}
	now := r.now()
	for i := range recs {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec := recs[i]
		switch {
		case rec.EnqueueAttempts >= r.cfg.MaxAttempts:
			// Budget spent: escalate to terminal, regardless of NextAttemptAt —
			// a record that will never be retried should not wait out a backoff.
			r.escalate(rec)
		case rec.ImageRepository == "" || rec.ToolName == "":
			// Pre-feature record without the replay fields: inert. Never
			// retried (the request cannot be rebuilt) and never escalated
			// (leave the historical record untouched — no backfill).
			continue
		case !rec.NextAttemptAt.IsZero() && rec.NextAttemptAt.After(now):
			continue // still backing off
		default:
			r.retryOne(ctx, rec)
		}
	}
	return nil
}

// escalate drives Unavailable -> EnqueueAbandoned (terminal). An
// ErrInvalidTransition here means a result raced in and moved the record off
// Unavailable, so there is nothing to escalate.
//
//nolint:gocritic // hugeParam: ValidationRequestRecord by value is intentional (snapshot).
func (r *EnqueueRetrier) escalate(rec index.ValidationRequestRecord) {
	reason := fmt.Sprintf("enqueue abandoned after %d failed attempts", rec.EnqueueAttempts)
	if rec.FailureReason != "" {
		reason += "; last error: " + rec.FailureReason
	}
	completedAt := r.now()
	err := r.store.TransitionValidationRequest(
		rec.ValidationRequestID, index.ValidationEnqueueAbandoned,
		func(rr *index.ValidationRequestRecord) {
			rr.FailureReason = reason
			rr.CompletedAt = completedAt
		},
	)
	switch {
	case err == nil:
		metrics.SentinelEnqueueAbandonedTotal.Add(1)
		slog.Warn("NodeSentinel enqueue abandoned (attempt budget exhausted)",
			"validation_request_id", rec.ValidationRequestID, "attempts", rec.EnqueueAttempts)
	case errors.Is(err, index.ErrInvalidTransition):
		// Raced with a result; nothing to escalate.
	default:
		slog.Warn("index: failed to escalate validation request to abandoned",
			"validation_request_id", rec.ValidationRequestID, "err", err)
	}
}

// retryOne re-sends one enqueue, reusing rec.ValidationRequestID.
//
//nolint:gocritic // hugeParam: ValidationRequestRecord by value is intentional (snapshot).
func (r *EnqueueRetrier) retryOne(ctx context.Context, rec index.ValidationRequestRecord) {
	attempt := rec.EnqueueAttempts + 1

	// Mark the attempt in-flight (Unavailable -> EnqueuePending) and count it up
	// front, so a crash mid-enqueue still consumes budget rather than allowing
	// unbounded retries. An ErrInvalidTransition means a result raced this
	// record forward; skip it.
	if err := r.store.TransitionValidationRequest(
		rec.ValidationRequestID, index.ValidationEnqueuePending,
		func(rr *index.ValidationRequestRecord) { rr.EnqueueAttempts = attempt },
	); err != nil {
		if errors.Is(err, index.ErrInvalidTransition) {
			return
		}
		slog.Warn("index: failed to arm enqueue retry",
			"validation_request_id", rec.ValidationRequestID, "err", err)
		return
	}

	enqReq := &nsv1.EnqueueValidationWorkRequest{
		ArtifactKind:        rec.ArtifactKind,
		ImageRepository:     rec.ImageRepository,
		ImageDigest:         rec.ImageDigest,
		ToolName:            rec.ToolName,
		Version:             rec.Version,
		CasHash:             rec.CasHash,
		RequestedActions:    rec.RequestedActions, // replayed verbatim — nothing added
		ValidationRequestId: rec.ValidationRequestID,
	}
	enqCtx, cancel := context.WithTimeout(ctx, r.cfg.EnqueueTimeout)
	defer cancel()
	resp, err := r.sentinel.EnqueueValidationWork(enqCtx, enqReq)
	if err != nil {
		r.onRetryFailure(rec.ValidationRequestID, attempt, err)
		return
	}
	r.onRetrySuccess(rec.ValidationRequestID, resp)
}

func (r *EnqueueRetrier) onRetrySuccess(id string, resp *nsv1.EnqueueValidationWorkResponse) {
	// The enqueue call itself succeeded — that is what this counter tracks,
	// independent of the bookkeeping transition below.
	metrics.SentinelEnqueueRetrySuccessTotal.Add(1)
	jobID := resp.GetJobId()
	queuedAt := r.now()
	err := r.store.TransitionValidationRequest(
		id, index.ValidationQueued,
		func(rr *index.ValidationRequestRecord) {
			rr.SentinelJobID = jobID
			rr.QueuedAt = queuedAt
			rr.FailureReason = ""
			rr.NextAttemptAt = time.Time{}
		},
	)
	switch {
	case err == nil:
		slog.Info("NodeSentinel enqueue retry succeeded",
			"validation_request_id", id, "job_id", jobID)
	case errors.Is(err, index.ErrInvalidTransition):
		// A result arrived first and drove EnqueuePending -> Running: the retry
		// still succeeded, the record simply already moved past Queued.
		slog.Info("NodeSentinel enqueue retry succeeded; record already progressed past Queued",
			"validation_request_id", id)
	default:
		slog.Warn("index: failed to mark retried validation request queued",
			"validation_request_id", id, "err", err)
	}
}

func (r *EnqueueRetrier) onRetryFailure(id string, attempt int, enqErr error) {
	metrics.SentinelEnqueueRetryFailureTotal.Add(1)
	nextAt := r.now().Add(r.backoff(attempt))
	slog.Warn("NodeSentinel enqueue retry failed (will retry after backoff)",
		"validation_request_id", id, "attempt", attempt, "next_attempt_at", nextAt, "err", enqErr)
	err := r.store.TransitionValidationRequest(
		id, index.ValidationUnavailable,
		func(rr *index.ValidationRequestRecord) {
			rr.FailureReason = enqErr.Error()
			rr.NextAttemptAt = nextAt
		},
	)
	// ErrInvalidTransition here means a result raced the record off
	// EnqueuePending; no retry needs rescheduling in that case.
	if err != nil && !errors.Is(err, index.ErrInvalidTransition) {
		slog.Warn("index: failed to reschedule validation request retry",
			"validation_request_id", id, "err", err)
	}
}

// backoff returns the delay before the next attempt given how many attempts
// have been made: BaseBackoff doubled per attempt, capped at MaxBackoff, then
// spread by ±JitterFrac. attempt is >= 1. The doubling loop stops at the cap so
// it cannot overflow.
func (r *EnqueueRetrier) backoff(attempt int) time.Duration {
	d := r.cfg.BaseBackoff
	for i := 1; i < attempt; i++ {
		if d >= r.cfg.MaxBackoff/2 {
			d = r.cfg.MaxBackoff
			break
		}
		d *= 2
	}
	if d > r.cfg.MaxBackoff {
		d = r.cfg.MaxBackoff
	}
	// jitter in [-JitterFrac, +JitterFrac] of d.
	delta := (r.jitter()*2 - 1) * r.cfg.JitterFrac
	d += time.Duration(float64(d) * delta)
	if d < 0 {
		d = 0
	}
	return d
}
