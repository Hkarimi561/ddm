package engine

import (
	"crypto/sha256"
	"encoding"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"sync"
)

// chunk is a byte range of a blob that is fetched sequentially by one
// connection at a time. It keeps a running sha256 of the bytes as they were
// received, which lets verification pinpoint chunks that got corrupted on
// disk instead of throwing the whole blob away.
type chunk struct {
	start, end int64 // [start, end) within the blob

	mu   sync.Mutex
	done int64     // bytes of the range already written, counted from start
	h    hash.Hash // sha256 of those bytes
}

func newChunk(start, end int64) *chunk {
	return &chunk{start: start, end: end, h: sha256.New()}
}

// chunkFromState restores a persisted chunk. If its hash state can't be
// restored the chunk starts over, since its bytes could no longer be checked.
func chunkFromState(s ChunkState) *chunk {
	c := newChunk(s.Start, s.End)
	if s.Done > 0 {
		h := sha256.New()
		if u, ok := h.(encoding.BinaryUnmarshaler); ok && u.UnmarshalBinary(s.Hash) == nil {
			c.h, c.done = h, s.Done
		}
	}
	return c
}

func (c *chunk) state() ChunkState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := ChunkState{Start: c.start, End: c.end, Done: c.done}
	if c.done > 0 {
		if m, ok := c.h.(encoding.BinaryMarshaler); ok {
			s.Hash, _ = m.MarshalBinary()
		}
	}
	return s
}

// cursor returns the blob offset of the next byte to fetch.
func (c *chunk) cursor() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start + c.done
}

func (c *chunk) doneBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

func (c *chunk) complete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start+c.done >= c.end
}

// advance records that p was written at the chunk's cursor.
func (c *chunk) advance(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.h.Write(p)
	c.done += int64(len(p))
}

func (c *chunk) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = 0
	c.h = sha256.New()
}

func (c *chunk) sum() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.h.Sum(nil)
}

type span struct{ start, end int64 }

// planChunks splits [0, size) into contiguous chunks so that conns
// connections can work in parallel. Chunk sizes stay within [minChunk,
// maxChunk]; capping them bounds how much is lost when a chunk turns out to
// be corrupt, and lets idle connections pick up remaining work.
func planChunks(size int64, conns int, minChunk, maxChunk int64) []span {
	if size <= 0 {
		return nil
	}
	conns = max(conns, 1)
	cs := (size + int64(conns) - 1) / int64(conns)
	cs = max(min(cs, maxChunk), minChunk)
	var out []span
	for s := int64(0); s < size; s += cs {
		out = append(out, span{s, min(s+cs, size)})
	}
	return out
}

// parseContentRange parses a Content-Range header like "bytes 0-99/1000".
// total is -1 when the server sends "*".
func parseContentRange(v string) (first, last, total int64, err error) {
	bad := fmt.Errorf("malformed Content-Range %q", v)
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, 0, 0, bad
	}
	rng, tot, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, bad
	}
	a, b, ok := strings.Cut(rng, "-")
	if !ok {
		return 0, 0, 0, bad
	}
	if first, err = strconv.ParseInt(strings.TrimSpace(a), 10, 64); err != nil {
		return 0, 0, 0, bad
	}
	if last, err = strconv.ParseInt(strings.TrimSpace(b), 10, 64); err != nil || last < first {
		return 0, 0, 0, bad
	}
	total = -1
	if tot = strings.TrimSpace(tot); tot != "*" {
		if total, err = strconv.ParseInt(tot, 10, 64); err != nil {
			return 0, 0, 0, bad
		}
	}
	return first, last, total, nil
}
