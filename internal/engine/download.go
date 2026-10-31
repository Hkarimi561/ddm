package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sync/errgroup"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/retry"
)

const (
	readBufferSize = 256 << 10
	maxRounds      = 10 // re-plans (range fallback, corrupt chunks) per Download
	maxMismatches  = 3  // full re-downloads after a digest mismatch
	maxAuthRetries = 5  // 401/403 from a redirect target before giving up
)

// Downloader fetches blobs into a cache.
type Downloader struct {
	Cache    *cache.Cache
	Options  Options
	Progress Progress // optional
	Log      *slog.Logger

	// beforeVerify, when set, runs after all chunks are fetched and before
	// the digest is checked. Tests use it to corrupt data on disk.
	beforeVerify func(path string)
}

// Download fetches b into the cache from srcs, tried in order (see
// sources), resuming any earlier partial download of the same digest. It
// returns nil once the verified blob is in the cache. Concurrent downloads of
// the same blob (in this or another process) wait for each other instead of
// clobbering the data.
func (d *Downloader) Download(ctx context.Context, b Blob, srcs []Source) error {
	if _, err := cache.ParseDigest(b.Digest); err != nil {
		return err
	}
	if len(srcs) == 0 {
		return fmt.Errorf("no source for blob %s", b.Digest)
	}
	prog := d.Progress
	if prog == nil {
		prog = nopProgress{}
	}
	log := d.Log
	if log == nil {
		log = discardLogger()
	}
	log = log.With("blob", ShortDigest(b.Digest))
	label := b.Name
	if label == "" {
		label = ShortDigest(b.Digest)
	}

	cached := func() bool {
		if !d.Cache.Has(b.Digest, b.Size) {
			return false
		}
		prog.Start(b, b.Size)
		prog.Done(b.Digest)
		return true
	}
	if cached() {
		return nil
	}
	switch {
	case b.Size == 0:
		return d.Cache.PutBytes(b.Digest, nil)
	case b.Size < 0:
		return fmt.Errorf("%s: blob size is unknown", label)
	}

	paths := d.Cache.Partial(b.Digest)
	lock := flock.New(paths.Lock)
	locked, err := lock.TryLock()
	if err == nil && !locked {
		log.Info("waiting for another ddm process that is downloading the same blob")
		locked, err = lock.TryLockContext(ctx, 250*time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("%s: locking %s: %w", label, paths.Lock, err)
	}
	if !locked {
		return ctx.Err()
	}
	// The lock file itself is left in place: deleting it would let a process
	// that already opened it lock a file nobody else can see anymore.
	defer func() { _ = lock.Unlock() }()

	// Another process may have finished the blob while we waited.
	if cached() {
		return nil
	}

	opts := d.Options.withDefaults()
	srcSet := newSources(srcs, opts.SwitchAfter, func(to Source, reason string) {
		log.Info("switching source", "to", to.Name, "because", reason)
		prog.Source(b.Digest, to.Name)
	})
	defer srcSet.close()
	j := &job{d: d, opts: opts, blob: b, srcs: srcSet, paths: paths, prog: prog, log: log}

	mismatches := 0
	for round := 1; ; round++ {
		err := j.run(ctx)
		switch {
		case err == nil:
			prog.Done(b.Digest)
			return nil
		case errors.Is(err, errAgain) && round < maxRounds:
			continue
		case errors.Is(err, ErrDigestMismatch):
			if mismatches++; mismatches < maxMismatches {
				log.Warn("downloaded data does not match its digest; downloading it again", "err", err)
				srcSet.rotate("sent data that failed verification")
				continue
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		prog.Fail(b.Digest, err)
		return fmt.Errorf("%s: %w", label, err)
	}
}

// job is the state of one blob download.
type job struct {
	d     *Downloader
	opts  Options
	blob  Blob
	srcs  *sources
	paths cache.Partial
	prog  Progress
	log   *slog.Logger

	file     *os.File
	chunks   []*chunk
	received atomic.Int64 // bytes received in this process, for speed checks
}

// run makes one pass: restore or plan the chunks, fetch what is missing,
// verify, and commit. errAgain means the plan changed and run should be
// called again.
func (j *job) run(ctx context.Context) error {
	if err := j.open(); err != nil {
		return err
	}
	defer j.closeFile()

	j.prog.Start(j.blob, j.completed())
	src, _, _ := j.srcs.current()
	j.prog.Source(j.blob.Digest, src.Name)
	if pending := j.pending(); len(pending) > 0 {
		err := j.fetchAll(ctx, pending)
		if errors.Is(err, errRangeUnsupported) {
			j.log.Info("server ignores range requests; falling back to a single stream")
			j.chunks = []*chunk{newChunk(0, j.blob.Size)}
			if err := j.persist(); err != nil {
				return err
			}
			return errAgain
		}
		if err != nil {
			return err
		}
	}
	if j.d.beforeVerify != nil {
		j.d.beforeVerify(j.paths.Data)
	}
	return j.verifyAndCommit()
}

func (j *job) open() error {
	f, err := os.OpenFile(j.paths.Data, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	j.file = f
	if j.chunks = j.loadChunks(); j.chunks != nil {
		return nil
	}

	// Fresh start: size the data file and plan the chunks.
	if err := makeSparse(f); err != nil {
		j.log.Debug("could not mark the download file sparse", "err", err)
	}
	if err := f.Truncate(0); err != nil {
		j.closeFile()
		return err
	}
	if err := f.Truncate(j.blob.Size); err != nil {
		j.closeFile()
		return err
	}
	for _, s := range planChunks(j.blob.Size, j.opts.Connections, j.opts.MinChunkSize, j.opts.MaxChunkSize) {
		j.chunks = append(j.chunks, newChunk(s.start, s.end))
	}
	if err := j.persist(); err != nil {
		j.closeFile()
		return err
	}
	return nil
}

// loadChunks restores progress from the state file, or returns nil when there
// is no usable state for this blob.
func (j *job) loadChunks() []*chunk {
	fi, err := j.file.Stat()
	if err != nil || fi.Size() != j.blob.Size {
		return nil
	}
	st, err := loadState(j.paths.State)
	if err != nil {
		return nil
	}
	if err := st.validate(j.blob.Digest, j.blob.Size); err != nil {
		j.log.Debug("discarding download state", "err", err)
		return nil
	}
	chunks := make([]*chunk, 0, len(st.Chunks))
	for _, cs := range st.Chunks {
		chunks = append(chunks, chunkFromState(cs))
	}
	return chunks
}

func (j *job) closeFile() {
	if j.file != nil {
		_ = j.file.Close()
		j.file = nil
	}
}

func (j *job) completed() int64 {
	var n int64
	for _, c := range j.chunks {
		n += c.doneBytes()
	}
	return n
}

func (j *job) pending() []*chunk {
	var p []*chunk
	for _, c := range j.chunks {
		if !c.complete() {
			p = append(p, c)
		}
	}
	return p
}

// persist saves the current progress. The chunks are snapshotted before the
// data file is fsynced, so every byte the snapshot claims is on disk by the
// time the state file is replaced.
func (j *job) persist() error {
	st := &State{Version: stateVersion, Digest: j.blob.Digest, Size: j.blob.Size}
	for _, c := range j.chunks {
		st.Chunks = append(st.Chunks, c.state())
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	return saveState(j.paths.State, st)
}

// fetchAll downloads the pending chunks with up to Connections workers while
// periodically persisting progress and watching the transfer speed.
func (j *job) fetchAll(ctx context.Context, pending []*chunk) error {
	queue := make(chan *chunk, len(pending))
	for _, c := range pending {
		queue <- c
	}
	close(queue)

	g, gctx := errgroup.WithContext(ctx)
	for range min(j.opts.Connections, len(pending)) {
		g.Go(func() error {
			for c := range queue {
				if err := j.fetchChunk(gctx, c); err != nil {
					return err
				}
			}
			return nil
		})
	}

	stop, monitorDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(monitorDone)
		j.monitor(stop)
	}()

	err := g.Wait()
	close(stop)
	<-monitorDone
	// Always save on the way out, including on Ctrl+C, so the next run
	// resumes exactly where this one stopped.
	if perr := j.persist(); perr != nil {
		j.log.Warn("could not save download progress", "err", perr)
	}
	return err
}

// monitor persists progress every SaveInterval and, when there is more than
// one source, switches away from a source that stays below MinSpeed for a
// whole SlowWindow.
func (j *job) monitor(stop <-chan struct{}) {
	save := time.NewTicker(j.opts.SaveInterval)
	defer save.Stop()
	speedCheck := j.opts.MinSpeed > 0 && len(j.srcs.list) > 1
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	windowStart, windowBytes, windowGen := time.Now(), j.received.Load(), j.srcs.generation()
	for {
		select {
		case <-stop:
			return
		case <-save.C:
			if err := j.persist(); err != nil {
				j.log.Warn("could not save download progress", "err", err)
			}
		case now := <-tick.C:
			if !speedCheck {
				continue
			}
			if limit := j.opts.Limiter.Rate(); j.opts.Gate.Paused() || (limit > 0 && limit < j.opts.MinSpeed) {
				// Paused, or capped below min_speed by the user: slowness
				// says nothing about the mirror.
				windowStart, windowBytes = now, j.received.Load()
				continue
			}
			if gen := j.srcs.generation(); gen != windowGen {
				// A new source gets a full window before it is judged.
				windowStart, windowBytes, windowGen = now, j.received.Load(), gen
				continue
			}
			elapsed := now.Sub(windowStart)
			if elapsed < j.opts.SlowWindow {
				continue
			}
			got := j.received.Load()
			speed := float64(got-windowBytes) / elapsed.Seconds()
			if speed < float64(j.opts.MinSpeed) {
				j.log.Debug("source below minimum speed", "bytes_per_sec", int64(speed), "min", j.opts.MinSpeed)
				j.srcs.tooSlow()
			}
			windowStart, windowBytes, windowGen = now, got, j.srcs.generation()
		}
	}
}

// fetchChunk downloads the rest of c, retrying transient failures with
// backoff and moving between sources as they fail. Attempts that made
// progress reset the failure count, so a flaky connection keeps going as
// long as data trickles through.
func (j *job) fetchChunk(ctx context.Context, c *chunk) error {
	failures, authFailures := 0, 0
	for !c.complete() {
		if err := j.opts.Gate.Wait(ctx); err != nil {
			return err
		}
		progressed, gen, err := j.fetchOnce(ctx, c)
		if progressed {
			j.srcs.progressed(gen)
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errSwitched) || errors.Is(err, errPaused) {
			continue // a pause is waited out at the top of the loop
		}
		if errors.Is(err, errRangeUnsupported) {
			return err
		}
		var pe *permanentError
		if errors.As(err, &pe) {
			if !pe.local && j.srcs.unavailable(gen) {
				failures = 0
				continue
			}
			return err
		}
		if progressed {
			failures = 0
		}
		failures++
		var se *statusError
		isStatus := errors.As(err, &se)
		if isStatus && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
			if authFailures++; authFailures > maxAuthRetries {
				return err
			}
		}
		if j.opts.MaxRetries > 0 && failures > j.opts.MaxRetries {
			return fmt.Errorf("giving up after %d attempts: %w", failures, err)
		}
		j.prog.Retry(j.blob.Digest, failures, err)
		if j.srcs.failed(gen) {
			continue // a fresh source deserves an immediate try
		}
		delay := retry.Backoff(failures, j.opts.BackoffBase, j.opts.BackoffMax)
		if isStatus && se.retryAfter > delay {
			delay = se.retryAfter
		}
		j.log.Debug("segment failed; retrying", "offset", c.cursor(), "attempt", failures,
			"delay", delay.Round(time.Millisecond), "err", err)
		if err := retry.Sleep(ctx, delay); err != nil {
			return err
		}
	}
	return nil
}

// fetchOnce issues one request for the rest of c to the current source and
// writes what arrives. progressed reports whether any bytes were stored; gen
// is the source generation the request used.
func (j *job) fetchOnce(ctx context.Context, c *chunk) (progressed bool, gen int, err error) {
	src, gen, genCtx := j.srcs.current()
	start, end := c.cursor(), c.end
	whole := c.start == 0 && c.end == j.blob.Size

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopOnSwitch := context.AfterFunc(genCtx, cancel)
	defer stopOnSwitch()
	pauseCtx := j.opts.Gate.context()
	stopOnPause := context.AfterFunc(pauseCtx, cancel)
	defer stopOnPause()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(j.opts.StallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()
	transient := func(err error) error {
		switch {
		case pauseCtx.Err() != nil:
			return errPaused
		case genCtx.Err() != nil:
			return errSwitched
		case stalled.Load():
			return fmt.Errorf("%w: no data received from %s for %s", errStalled, src.Name, j.opts.StallTimeout)
		}
		return err
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, src.URL, nil)
	if err != nil {
		return false, gen, &permanentError{err: err}
	}
	// A whole-blob download from the beginning is a plain GET, which also
	// works with servers that know nothing about ranges.
	ranged := !(whole && start == 0)
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	}
	resp, err := src.Client.Do(req)
	if err != nil {
		return false, gen, transient(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		first, _, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return false, gen, err
		}
		if first != start {
			return false, gen, fmt.Errorf("%s sent a range starting at %d instead of %d", src.Name, first, start)
		}
		if total >= 0 && total != j.blob.Size {
			return false, gen, &permanentError{err: fmt.Errorf("%s reports a size of %d bytes, expected %d", src.Name, total, j.blob.Size)}
		}
	case http.StatusOK:
		if ranged {
			if !whole {
				return false, gen, errRangeUnsupported
			}
			// Single-stream mode and the server ignored the resume offset:
			// the body starts at byte 0, so start over.
			c.reset()
			j.prog.Set(j.blob.Digest, j.completed())
			start = 0
		}
	default:
		return false, gen, classifyStatus(resp, req.URL.Host)
	}

	buf := make([]byte, readBufferSize)
	for off := start; off < end; {
		readSize := j.opts.Limiter.ReadSize(len(buf))
		n, rerr := resp.Body.Read(buf[:min(int64(readSize), end-off)])
		if n > 0 {
			watchdog.Reset(j.opts.StallTimeout)
			if _, werr := j.file.WriteAt(buf[:n], off); werr != nil {
				return progressed, gen, &permanentError{err: fmt.Errorf("writing download data: %w", werr), local: true}
			}
			c.advance(buf[:n])
			off += int64(n)
			progressed = true
			j.received.Add(int64(n))
			j.prog.Add(j.blob.Digest, int64(n))
			if err := j.opts.Limiter.Wait(reqCtx, n); err != nil {
				return progressed, gen, transient(err)
			}
			watchdog.Reset(j.opts.StallTimeout) // time spent throttled isn't a stall
		}
		if rerr != nil {
			if off >= end {
				break
			}
			if rerr == io.EOF {
				rerr = io.ErrUnexpectedEOF
			}
			return progressed, gen, transient(rerr)
		}
	}
	return progressed, gen, nil
}

// verifyAndCommit checks the blob digest and moves it into the cache. On a
// mismatch it compares every chunk with the hash recorded while it was
// received: chunks that changed on disk are re-fetched on their own; if none
// changed, a source itself sent bad data and the blob starts over.
func (j *job) verifyAndCommit() error {
	want := strings.TrimPrefix(j.blob.Digest, "sha256:")
	full := sha256.New()
	buf := make([]byte, 1<<20)
	var bad []*chunk
	for _, c := range j.chunks {
		h := sha256.New()
		r := io.NewSectionReader(j.file, c.start, c.end-c.start)
		if _, err := io.CopyBuffer(io.MultiWriter(full, h), r, buf); err != nil {
			return fmt.Errorf("verifying download: %w", err)
		}
		if !bytes.Equal(h.Sum(nil), c.sum()) {
			bad = append(bad, c)
		}
	}
	got := hex.EncodeToString(full.Sum(nil))

	if got == want {
		if err := j.file.Sync(); err != nil {
			return err
		}
		j.closeFile()
		if err := j.d.Cache.Commit(j.paths.Data, j.blob.Digest); err != nil {
			return fmt.Errorf("storing blob: %w", err)
		}
		_ = os.Remove(j.paths.State)
		return nil
	}

	if len(bad) > 0 {
		j.log.Warn("data on disk was corrupted; downloading the affected segments again", "segments", len(bad))
		for _, c := range bad {
			c.reset()
		}
		if err := j.persist(); err != nil {
			return err
		}
		return errAgain
	}

	j.closeFile()
	_ = os.Remove(j.paths.State)
	_ = os.Remove(j.paths.Data)
	return fmt.Errorf("%w: expected sha256:%s, got sha256:%s", ErrDigestMismatch, want, got)
}
