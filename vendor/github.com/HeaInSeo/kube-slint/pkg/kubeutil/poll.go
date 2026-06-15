package kubeutil

import (
	"context"
	"errors"
	"time"
)

// pollUntil calls fn immediately, then every interval, until fn returns (true, nil)
// or ctx is cancelled.
//
// fn returning (false, nil)     — not ready yet; keep retrying.
// fn returning (false, non-nil) — transient error; keep retrying, remember error.
// fn returning (true, nil)      — success; return nil.
// ctx cancellation              — return last fn error if any, otherwise ctx.Err().
func pollUntil(ctx context.Context, interval time.Duration, fn func() (bool, error)) error {
	var lastErr error

	tryOnce := func() bool {
		ok, err := fn()
		if err != nil {
			lastErr = err
		}
		return ok
	}

	if tryOnce() {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return errors.Join(lastErr, ctx.Err())
			}
			return ctx.Err()
		case <-ticker.C:
			if tryOnce() {
				return nil
			}
		}
	}
}
