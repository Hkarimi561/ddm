package engine

import (
	"context"
	"sync"
)

// sources decides which Source a blob is fetched from. It starts with the
// first one and moves on when the current source keeps failing, cannot
// serve the blob at all, or is too slow. Download state is keyed by digest
// alone, so whatever was fetched from one source is kept when switching.
//
// Every switch starts a new generation and cancels the requests of the old
// one, so all connections move over together.
type sources struct {
	list        []Source
	switchAfter int
	onSwitch    func(to Source, reason string)

	mu       sync.Mutex
	cur      int
	gen      int
	failures int    // consecutive failures of the current source
	dead     []bool // sources that cannot serve this blob
	slow     []bool // sources found too slow
	genCtx   context.Context
	cancel   context.CancelFunc
}

func newSources(list []Source, switchAfter int, onSwitch func(Source, string)) *sources {
	s := &sources{
		list:        list,
		switchAfter: switchAfter,
		onSwitch:    onSwitch,
		dead:        make([]bool, len(list)),
		slow:        make([]bool, len(list)),
	}
	s.genCtx, s.cancel = context.WithCancel(context.Background())
	return s
}

// current returns the source to use, its generation, and a context that is
// canceled when the source is switched away from.
func (s *sources) current() (Source, int, context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list[s.cur], s.gen, s.genCtx
}

func (s *sources) generation() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// progressed records that generation gen delivered data.
func (s *sources) progressed(gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen == s.gen {
		s.failures = 0
	}
}

// failed records a transient failure of generation gen and reports whether
// it made us switch to another source.
func (s *sources) failed(gen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return true // someone else already switched
	}
	s.failures++
	if s.failures < s.switchAfter {
		return false
	}
	return s.advanceLocked("keeps failing", func(int) bool { return false })
}

// unavailable marks the source of generation gen as unable to serve the
// blob (missing, access denied, wrong size). It reports whether another
// source is left to try.
func (s *sources) unavailable(gen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return true
	}
	s.dead[s.cur] = true
	return s.advanceLocked("does not have this blob", func(int) bool { return false })
}

// tooSlow marks the current source as slow and switches to one that hasn't
// been found slow yet. Once every source has been tried, it stays put rather
// than bouncing between equally slow mirrors.
func (s *sources) tooSlow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slow[s.cur] = true
	return s.advanceLocked("is too slow", func(i int) bool { return s.slow[i] })
}

// rotate moves to the next usable source, e.g. after a source delivered
// data that failed verification.
func (s *sources) rotate(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advanceLocked(reason, func(int) bool { return false })
}

// advanceLocked switches to the next source after the current one (wrapping
// around) that is not dead and not skipped, and reports whether it did. The
// onSwitch callback runs under the lock and must not call back into s.
func (s *sources) advanceLocked(reason string, skip func(int) bool) bool {
	for step := 1; step < len(s.list); step++ {
		i := (s.cur + step) % len(s.list)
		if s.dead[i] || skip(i) {
			continue
		}
		from := s.list[s.cur]
		s.cur = i
		s.gen++
		s.failures = 0
		s.cancel()
		s.genCtx, s.cancel = context.WithCancel(context.Background())
		if s.onSwitch != nil {
			s.onSwitch(s.list[i], from.Name+" "+reason)
		}
		return true
	}
	return false
}

func (s *sources) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel()
}
