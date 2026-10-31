package ui

import (
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/Hkarimi561/ddm/internal/engine"
)

const ewmaAge = 30

// bars shows one bar per blob plus a total bar at the bottom.
type bars struct {
	p     *mpb.Progress
	total *mpb.Bar

	mu        sync.Mutex
	blobs     map[string]*blobBar
	sum       int64
	totalLast time.Time
	failed    bool
}

type blobBar struct {
	bar      *mpb.Bar
	current  int64
	last     time.Time
	retries  atomic.Int64
	finished bool
}

func newBars(w io.Writer, total int64) *bars {
	p := mpb.New(mpb.WithOutput(w), mpb.WithWidth(30), mpb.WithRefreshRate(200*time.Millisecond))
	r := &bars{p: p, blobs: map[string]*blobBar{}, totalLast: time.Now()}
	r.total = p.AddBar(total,
		mpb.BarPriority(math.MaxInt32),
		mpb.PrependDecorators(decor.Name(label("total"))),
		mpb.AppendDecorators(
			decor.CountersKibiByte("% .1f / % .1f"),
			decor.Name("  "),
			decor.EwmaSpeed(decor.SizeB1024(0), "% .1f", ewmaAge),
			decor.Name("  ETA "),
			decor.EwmaETA(decor.ET_STYLE_GO, ewmaAge),
		),
	)
	return r
}

func label(s string) string { return fmt.Sprintf("%-26.26s", s) }

func (r *bars) Start(b engine.Blob, current int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bb := r.blobs[b.Digest]
	if bb == nil {
		bb = &blobBar{}
		bb.bar = r.p.AddBar(b.Size,
			mpb.PrependDecorators(
				decor.Name(label(b.Name)),
				decor.Any(func(decor.Statistics) string {
					if n := bb.retries.Load(); n > 0 {
						return fmt.Sprintf(" retries:%d", n)
					}
					return ""
				}),
			),
			mpb.AppendDecorators(
				decor.CountersKibiByte("% .1f / % .1f"),
				decor.Name("  "),
				decor.OnComplete(decor.EwmaSpeed(decor.SizeB1024(0), "% .1f", ewmaAge), "done"),
			),
		)
		r.blobs[b.Digest] = bb
	}
	r.setLocked(bb, current)
}

func (r *bars) setLocked(bb *blobBar, current int64) {
	r.sum += current - bb.current
	bb.current = current
	bb.last = time.Now()
	bb.bar.SetCurrent(current)
	r.total.SetCurrent(r.sum)
}

func (r *bars) Add(digest string, n int64) {
	r.mu.Lock()
	bb := r.blobs[digest]
	if bb == nil {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	dur, totalDur := now.Sub(bb.last), now.Sub(r.totalLast)
	bb.last, r.totalLast = now, now
	bb.current += n
	r.sum += n
	r.mu.Unlock()
	bb.bar.EwmaIncrInt64(n, dur)
	r.total.EwmaIncrInt64(n, totalDur)
}

func (r *bars) Set(digest string, current int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bb := r.blobs[digest]; bb != nil {
		r.setLocked(bb, current)
	}
}

func (r *bars) Retry(digest string, _ int, _ error) {
	r.mu.Lock()
	bb := r.blobs[digest]
	r.mu.Unlock()
	if bb != nil {
		bb.retries.Add(1)
	}
}

func (r *bars) Done(digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bb := r.blobs[digest]; bb != nil && !bb.finished {
		bb.finished = true
		bb.bar.SetTotal(-1, true)
	}
}

func (r *bars) Fail(digest string, _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
	if bb := r.blobs[digest]; bb != nil && !bb.finished {
		bb.finished = true
		bb.bar.Abort(false)
	}
}

// Close stops every unfinished bar (e.g. after Ctrl+C) and waits for the
// final render.
func (r *bars) Close() {
	r.mu.Lock()
	ok := !r.failed
	for _, bb := range r.blobs {
		if !bb.finished {
			ok = false
			bb.finished = true
			bb.bar.Abort(false)
		}
	}
	if ok {
		r.total.SetTotal(-1, true)
	} else {
		r.total.Abort(false)
	}
	r.mu.Unlock()
	r.p.Wait()
}
