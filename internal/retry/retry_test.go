package retry

import (
	"context"
	"testing"
	"time"
)

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	base, maxDelay := 100*time.Millisecond, 3*time.Second
	for attempt := 1; attempt <= 30; attempt++ {
		ceiling := base << (attempt - 1)
		if ceiling > maxDelay || ceiling <= 0 {
			ceiling = maxDelay
		}
		for range 50 {
			d := Backoff(attempt, base, maxDelay)
			if d < ceiling/2 || d > ceiling {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, d, ceiling/2, ceiling)
			}
		}
	}
}

func TestSleepStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Fatal("Sleep returned nil for a canceled context")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Sleep did not return promptly")
	}
}
