package engine

import (
	"context"
	"sync"
	"time"
)

// Limiter caps the combined speed of every connection that shares it.
// Connections reserve bandwidth for what they just read and sleep until the
// reservation has passed, which slows the TCP stream down by back-pressure.
// A nil *Limiter, or a rate of 0, means unlimited. Safe for concurrent use;
// the rate can change while downloads run.
type Limiter struct {
	mu   sync.Mutex
	rate float64   // bytes per second
	next time.Time // when the bandwidth reserved so far is used up
}

// NewLimiter returns a limiter for bytesPerSec (0 = unlimited).
func NewLimiter(bytesPerSec int64) *Limiter {
	l := &Limiter{}
	l.SetRate(bytesPerSec)
	return l
}

// SetRate changes the limit (0 = unlimited).
func (l *Limiter) SetRate(bytesPerSec int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = float64(max(bytesPerSec, 0))
}

// Rate returns the current limit in bytes per second (0 = unlimited).
func (l *Limiter) Rate() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.rate)
}

// ReadSize returns how much a connection should read at a time, so that a
// single read never has to wait long (which would also trip stall
// detection): about a quarter second's worth, between 4 KiB and max.
func (l *Limiter) ReadSize(maxSize int) int {
	r := l.Rate()
	if r <= 0 {
		return maxSize
	}
	return int(min(max(r/4, 4<<10), int64(maxSize)))
}

// Wait accounts for n bytes just received and sleeps as long as needed to
// keep the average at the limit.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	if l.next.Before(now) {
		l.next = now // idle time doesn't build up credit for a burst
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	wait := l.next.Sub(now)
	l.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Gate pauses and resumes downloads. Pausing cancels in-flight requests
// (their bytes are kept) and holds connections until Resume. A nil *Gate
// never pauses.
type Gate struct {
	mu       sync.Mutex
	paused   bool
	resumed  chan struct{} // closed on Resume
	pauseCtx context.Context
	cancel   context.CancelFunc // cancels pauseCtx on Pause
}

// NewGate returns an open gate.
func NewGate() *Gate {
	g := &Gate{}
	g.pauseCtx, g.cancel = context.WithCancel(context.Background())
	return g
}

// Pause stops downloads.
func (g *Gate) Pause() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return
	}
	g.paused = true
	g.resumed = make(chan struct{})
	g.cancel()
}

// Resume lets downloads continue.
func (g *Gate) Resume() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		return
	}
	g.paused = false
	close(g.resumed)
	g.pauseCtx, g.cancel = context.WithCancel(context.Background())
}

// Paused reports whether the gate is closed.
func (g *Gate) Paused() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// Wait blocks while the gate is paused.
func (g *Gate) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if !g.paused {
		g.mu.Unlock()
		return nil
	}
	ch := g.resumed
	g.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// context returns a context that is canceled when the gate pauses (one that
// never is, for a nil gate).
func (g *Gate) context() context.Context {
	if g == nil {
		return context.Background()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pauseCtx
}
