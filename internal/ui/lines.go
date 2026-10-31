package ui

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Hkarimi561/ddm/internal/engine"
)

const lineInterval = 10 * time.Second

// lines reports progress as plain log lines, for non-interactive output.
type lines struct {
	w io.Writer

	mu    sync.Mutex
	total int64
	sum   int64
	cur   map[string]int64
	names map[string]string

	stop, done chan struct{}
}

func newLines(w io.Writer, total int64) *lines {
	l := &lines{
		w: w, total: total,
		cur: map[string]int64{}, names: map[string]string{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go l.tick()
	return l
}

func (l *lines) tick() {
	defer close(l.done)
	t := time.NewTicker(lineInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			pct := 0.0
			if l.total > 0 {
				pct = 100 * float64(l.sum) / float64(l.total)
			}
			fmt.Fprintf(l.w, "progress: %s / %s (%.0f%%)\n", HumanBytes(l.sum), HumanBytes(l.total), pct)
			l.mu.Unlock()
		}
	}
}

func (l *lines) Start(b engine.Blob, current int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, seen := l.names[b.Digest]; !seen {
		l.names[b.Digest] = b.Name
		if current > 0 {
			fmt.Fprintf(l.w, "->  %s (%s, resuming at %s)\n", b.Name, HumanBytes(b.Size), HumanBytes(current))
		} else {
			fmt.Fprintf(l.w, "->  %s (%s)\n", b.Name, HumanBytes(b.Size))
		}
	}
	l.setLocked(b.Digest, current)
}

func (l *lines) setLocked(digest string, current int64) {
	l.sum += current - l.cur[digest]
	l.cur[digest] = current
}

func (l *lines) Add(digest string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur[digest] += n
	l.sum += n
}

func (l *lines) Set(digest string, current int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setLocked(digest, current)
}

func (l *lines) Retry(digest string, attempt int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "!   %s: attempt %d failed, retrying: %v\n", l.names[digest], attempt, err)
}

func (l *lines) Done(digest string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "ok  %s\n", l.names[digest])
}

func (l *lines) Fail(digest string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "x   %s: %v\n", l.names[digest], err)
}

func (l *lines) Close() {
	close(l.stop)
	<-l.done
}
