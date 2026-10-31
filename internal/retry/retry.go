// Package retry holds the backoff policy shared by every network operation.
package retry

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff returns how long to wait before retry number attempt (1-based):
// exponential growth from base, capped at maxDelay, with "equal jitter" so
// parallel connections that failed together don't retry in lockstep.
func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maxDelay < base {
		maxDelay = base
	}
	d := base
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	d = min(d, maxDelay)
	half := d / 2
	return half + rand.N(half+1)
}

// Sleep waits for d, returning early with ctx.Err() if ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
