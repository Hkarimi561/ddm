package engine

import (
	"context"
	"testing"
	"time"
)

func TestLimiterHoldsTheRate(t *testing.T) {
	l := NewLimiter(1 << 20) // 1 MiB/s
	start := time.Now()
	for range 8 {
		if err := l.Wait(context.Background(), 64<<10); err != nil {
			t.Fatal(err)
		}
	}
	// 512 KiB at 1 MiB/s takes about half a second.
	if d := time.Since(start); d < 400*time.Millisecond || d > 2*time.Second {
		t.Errorf("512 KiB took %v at 1 MiB/s", d)
	}
	if got := l.ReadSize(256 << 10); got != 256<<10 {
		t.Errorf("ReadSize = %d, want a quarter second's worth (256 KiB)", got)
	}

	var unlimited *Limiter
	if err := unlimited.Wait(context.Background(), 1<<30); err != nil || unlimited.ReadSize(100) != 100 {
		t.Error("a nil limiter must not limit")
	}
	l.SetRate(0)
	start = time.Now()
	_ = l.Wait(context.Background(), 1<<30)
	if time.Since(start) > 50*time.Millisecond {
		t.Error("rate 0 must mean unlimited")
	}
}

func TestLimiterWaitStopsOnCancel(t *testing.T) {
	l := NewLimiter(1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, 1<<20); err == nil {
		t.Fatal("Wait returned nil although the context expired")
	}
}

func TestGate(t *testing.T) {
	g := NewGate()
	if err := g.Wait(context.Background()); err != nil || g.Paused() {
		t.Fatal("a new gate must be open")
	}
	pauseCtx := g.context()
	g.Pause()
	if pauseCtx.Err() == nil {
		t.Fatal("pausing did not cancel in-flight work")
	}
	released := make(chan struct{})
	go func() {
		_ = g.Wait(context.Background())
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("Wait returned while paused")
	case <-time.After(50 * time.Millisecond):
	}
	g.Resume()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("Resume did not release waiters")
	}
	if g.context().Err() != nil {
		t.Fatal("after Resume, new work must not start canceled")
	}
	var none *Gate
	none.Pause()
	if none.Paused() || none.Wait(context.Background()) != nil {
		t.Error("a nil gate must never pause")
	}
}

func TestPausedDownloadResumesWithoutLosingData(t *testing.T) {
	f := newFixture(t, 2<<20)
	f.reg.Delay = 5 * time.Millisecond
	gate := NewGate()
	opts := testOptions()
	opts.Gate = gate
	paused := make(chan struct{})
	f.reg.OnServe(func(_ string, served int64) {
		if served >= 512<<10 && !gate.Paused() {
			gate.Pause()
			select {
			case <-paused:
			default:
				close(paused)
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- f.download(context.Background(), opts) }()

	<-paused
	time.Sleep(200 * time.Millisecond)
	f.reg.OnServe(nil)
	before := f.reg.TotalServed()
	time.Sleep(300 * time.Millisecond)
	if after := f.reg.TotalServed(); after-before > 256<<10 {
		t.Errorf("%d bytes were served while paused", after-before)
	}
	gate.Resume()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)
}
