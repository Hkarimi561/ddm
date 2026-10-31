// Package runstate lets ddm processes see and steer each other: every
// downloading process publishes its progress to a small JSON file in the
// cache's run directory, and `ddm pause` / `ddm resume` drop and remove
// marker files that the process notices within a second.
package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hkarimi561/ddm/internal/engine"
)

const (
	tick       = time.Second
	staleAfter = 10 * time.Second // a status not refreshed for this long belongs to a dead process
)

// Status is what a running process publishes.
type Status struct {
	PID         int       `json:"pid"`
	Command     string    `json:"command"`
	Started     time.Time `json:"started"`
	Updated     time.Time `json:"updated"`
	Paused      bool      `json:"paused"`
	Done        int64     `json:"done"`
	Total       int64     `json:"total"`
	BytesPerSec float64   `json:"bytes_per_sec"`
	RateLimit   int64     `json:"rate_limit,omitempty"`
	Items       []Item    `json:"items"`
}

// Item is one blob being downloaded.
type Item struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Done   int64  `json:"done"`
	Source string `json:"source,omitempty"`
	State  string `json:"state"` // downloading, done, failed
}

// Dir is a cache's run directory.
type Dir string

// Open returns the run directory of the cache at cacheRoot, creating it.
func Open(cacheRoot string) (Dir, error) {
	d := filepath.Join(cacheRoot, "run")
	return Dir(d), os.MkdirAll(d, 0o755)
}

func (d Dir) statusPath(pid int) string { return filepath.Join(string(d), strconv.Itoa(pid)+".json") }
func (d Dir) pausePath(pid int) string  { return filepath.Join(string(d), strconv.Itoa(pid)+".paused") }
func (d Dir) limitPath(pid int) string  { return filepath.Join(string(d), strconv.Itoa(pid)+".limit") }

// SetLimit changes the bandwidth limit of running processes (pid 0: all of
// them) to bytesPerSec (0 = unlimited). It returns the PIDs changed.
func (d Dir) SetLimit(pid int, bytesPerSec int64) ([]int, error) {
	return d.forEach(pid, func(p int) error {
		return os.WriteFile(d.limitPath(p), []byte(strconv.FormatInt(bytesPerSec, 10)), 0o644)
	})
}

// List returns the statuses of live processes, oldest first, and cleans up
// files left by processes that died.
func (d Dir) List() ([]Status, error) {
	entries, err := os.ReadDir(string(d))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Status
	live := map[int]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(string(d), e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Status
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		if time.Since(s.Updated) > staleAfter {
			_ = os.Remove(path)
			continue
		}
		live[s.PID] = true
		out = append(out, s)
	}
	for _, e := range entries {
		for _, suffix := range []string{".paused", ".limit"} {
			if pid, ok := strings.CutSuffix(e.Name(), suffix); ok {
				if n, err := strconv.Atoi(pid); err == nil && !live[n] {
					_ = os.Remove(filepath.Join(string(d), e.Name()))
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

// Pause asks running processes to pause: the one with pid, or every one if
// pid is 0. It returns the PIDs asked. Processes started later are not
// affected.
func (d Dir) Pause(pid int) ([]int, error) {
	return d.forEach(pid, func(p int) error {
		return os.WriteFile(d.pausePath(p), nil, 0o644)
	})
}

// Resume undoes Pause.
func (d Dir) Resume(pid int) ([]int, error) {
	return d.forEach(pid, func(p int) error {
		if err := os.Remove(d.pausePath(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

func (d Dir) forEach(pid int, fn func(int) error) ([]int, error) {
	runs, err := d.List()
	if err != nil {
		return nil, err
	}
	var done []int
	for _, s := range runs {
		if pid != 0 && s.PID != pid {
			continue
		}
		if err := fn(s.PID); err != nil {
			return done, err
		}
		done = append(done, s.PID)
	}
	if pid != 0 && len(done) == 0 {
		return nil, fmt.Errorf("no running ddm download with PID %d (see `ddm status`)", pid)
	}
	return done, nil
}

// Run is this process's entry in the run directory. It records download
// progress (it is an engine.Progress) and applies pause requests to gate.
type Run struct {
	dir     Dir
	gate    *engine.Gate
	limiter *engine.Limiter
	onPause func(paused bool)

	mu       sync.Mutex
	status   Status
	items    map[string]*Item
	order    []string
	lastDone int64
	lastTick time.Time

	stop chan struct{}
	done chan struct{}
}

// Start registers this process. onPause (optional) is told when a pause
// request arrives or is withdrawn.
func (d Dir) Start(command string, gate *engine.Gate, limiter *engine.Limiter, onPause func(bool)) (*Run, error) {
	now := time.Now()
	r := &Run{
		dir: d, gate: gate, limiter: limiter, onPause: onPause,
		status:   Status{PID: os.Getpid(), Command: command, Started: now, Updated: now},
		items:    map[string]*Item{},
		lastTick: now,
		stop:     make(chan struct{}), done: make(chan struct{}),
	}
	if err := r.write(); err != nil {
		return nil, err
	}
	go r.loop()
	return r, nil
}

// Close removes this process's files.
func (r *Run) Close() {
	close(r.stop)
	<-r.done
	_ = os.Remove(r.dir.statusPath(r.status.PID))
	_ = os.Remove(r.dir.pausePath(r.status.PID))
	_ = os.Remove(r.dir.limitPath(r.status.PID))
}

func (r *Run) loop() {
	defer close(r.done)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			_, err := os.Stat(r.dir.pausePath(r.status.PID))
			paused := err == nil
			if paused != r.gate.Paused() {
				if paused {
					r.gate.Pause()
				} else {
					r.gate.Resume()
				}
				if r.onPause != nil {
					r.onPause(paused)
				}
			}
			if b, err := os.ReadFile(r.dir.limitPath(r.status.PID)); err == nil {
				if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
					r.limiter.SetRate(n)
				}
				_ = os.Remove(r.dir.limitPath(r.status.PID))
			}
			_ = r.write()
		}
	}
}

func (r *Run) write() error {
	r.mu.Lock()
	now := time.Now()
	s := r.status
	s.Updated = now
	s.Paused = r.gate.Paused()
	s.RateLimit = r.limiter.Rate()
	if dt := now.Sub(r.lastTick).Seconds(); dt > 0 {
		inst := float64(s.Done-r.lastDone) / dt
		// Smooth like a progress bar would, but react quickly to a pause.
		if s.Paused {
			r.status.BytesPerSec = 0
		} else {
			r.status.BytesPerSec = 0.7*r.status.BytesPerSec + 0.3*inst
		}
		s.BytesPerSec = r.status.BytesPerSec
	}
	r.lastDone, r.lastTick = s.Done, now
	s.Items = make([]Item, 0, len(r.order))
	for _, d := range r.order {
		s.Items = append(s.Items, *r.items[d])
	}
	r.mu.Unlock()

	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(string(r.dir), "status-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), r.dir.statusPath(s.PID))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// The engine.Progress side: keep per-blob numbers up to date.

func (r *Run) item(digest string) *Item {
	it := r.items[digest]
	if it == nil {
		it = &Item{Digest: digest, State: "downloading"}
		r.items[digest] = it
		r.order = append(r.order, digest)
	}
	return it
}

func (r *Run) Start(b engine.Blob, current int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.item(b.Digest)
	if it.Size == 0 {
		r.status.Total += b.Size
	}
	it.Name, it.Size = b.Name, b.Size
	r.status.Done += current - it.Done
	it.Done = current
}

func (r *Run) Add(digest string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.item(digest).Done += n
	r.status.Done += n
}

func (r *Run) Set(digest string, current int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.item(digest)
	r.status.Done += current - it.Done
	it.Done = current
}

func (r *Run) Retry(string, int, error) {}

func (r *Run) Source(digest, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.item(digest).Source = name
}

func (r *Run) Done(digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.item(digest).State = "done"
}

func (r *Run) Fail(digest string, _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.item(digest).State = "failed"
}

// Reporter is a progress display (see pull.Reporter).
type Reporter interface {
	engine.Progress
	Close()
}

// Tee returns a Reporter that feeds both rep and r.
func (r *Run) Tee(rep Reporter) Reporter { return tee{rep, r} }

type tee struct {
	Reporter
	run *Run
}

func (t tee) Start(b engine.Blob, current int64) { t.Reporter.Start(b, current); t.run.Start(b, current) }
func (t tee) Add(d string, n int64)              { t.Reporter.Add(d, n); t.run.Add(d, n) }
func (t tee) Set(d string, current int64)        { t.Reporter.Set(d, current); t.run.Set(d, current) }
func (t tee) Retry(d string, a int, err error)   { t.Reporter.Retry(d, a, err); t.run.Retry(d, a, err) }
func (t tee) Source(d, name string)              { t.Reporter.Source(d, name); t.run.Source(d, name) }
func (t tee) Done(d string)                      { t.Reporter.Done(d); t.run.Done(d) }
func (t tee) Fail(d string, err error)           { t.Reporter.Fail(d, err); t.run.Fail(d, err) }
