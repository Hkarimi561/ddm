// Package testutil provides a fake, unreliable OCI registry for tests.
package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Manifest is a manifest served by FlakyRegistry.
type Manifest struct {
	MediaType string
	Body      []byte
}

// FlakyRegistry is a minimal OCI distribution server that can cut blob
// transfers off part-way, hang without sending data, or ignore Range
// headers. It counts the blob bytes it actually sent, so tests can assert
// that nothing was downloaded twice.
//
// Configure the exported fields before serving requests.
type FlakyRegistry struct {
	Repo      string
	Manifests map[string]Manifest // by tag (digests are looked up automatically)
	Blobs     map[string][]byte   // by digest

	DropRate  float64 // chance that a blob response is cut off mid-transfer
	HangRate  float64 // chance that a blob response stops sending data and hangs
	NoRange   bool    // ignore Range headers and always send the whole blob
	Redirect  bool    // serve blobs through a redirect, like CDN-backed registries
	PieceSize int     // bytes written and flushed at a time (default 16 KiB)

	mu         sync.Mutex
	rng        *rand.Rand
	generation int
	served     map[string]int64
	requests   int
	drops      int
	onServe    func(digest string, served int64)
}

// NewFlakyRegistry returns a registry serving repository repo. seed makes
// the failure pattern reproducible (up to goroutine scheduling).
func NewFlakyRegistry(repo string, seed int64) *FlakyRegistry {
	return &FlakyRegistry{
		Repo:      repo,
		Manifests: map[string]Manifest{},
		Blobs:     map[string][]byte{},
		rng:       rand.New(rand.NewSource(seed)),
		served:    map[string]int64{},
	}
}

// Digest returns the sha256 digest of b.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// AddBlob stores data under its digest and returns the digest.
func (f *FlakyRegistry) AddBlob(data []byte) string {
	d := Digest(data)
	f.Blobs[d] = data
	return d
}

// OnServe registers fn to run after every chunk of blob data is sent, with
// the number of bytes sent for that blob so far. Pass nil to remove it.
func (f *FlakyRegistry) OnServe(fn func(digest string, served int64)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServe = fn
}

// Reset zeroes the counters. Responses still in flight from before the reset
// are not counted afterwards.
func (f *FlakyRegistry) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generation++
	f.served = map[string]int64{}
	f.requests, f.drops = 0, 0
}

// TotalServed returns the blob bytes sent since the last Reset.
func (f *FlakyRegistry) TotalServed() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, v := range f.served {
		n += v
	}
	return n
}

// Requests returns the number of blob requests since the last Reset.
func (f *FlakyRegistry) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// Drops returns how many responses were cut off or hung since the last Reset.
func (f *FlakyRegistry) Drops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drops
}

func (f *FlakyRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/v2/" || p == "/v2" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if d, ok := strings.CutPrefix(p, "/cdn/"); ok {
		f.serveBlob(w, r, d)
		return
	}
	rest, ok := strings.CutPrefix(p, "/v2/"+f.Repo+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	kind, ref, _ := strings.Cut(rest, "/")
	switch kind {
	case "manifests":
		f.serveManifest(w, r, ref)
	case "blobs":
		if f.Redirect {
			http.Redirect(w, r, "/cdn/"+ref, http.StatusTemporaryRedirect)
			return
		}
		f.serveBlob(w, r, ref)
	default:
		http.NotFound(w, r)
	}
}

func (f *FlakyRegistry) serveManifest(w http.ResponseWriter, r *http.Request, ref string) {
	m, ok := f.Manifests[ref]
	if !ok {
		for _, mm := range f.Manifests {
			if Digest(mm.Body) == ref {
				m, ok = mm, true
				break
			}
		}
	}
	if !ok {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", m.MediaType)
	w.Header().Set("Docker-Content-Digest", Digest(m.Body))
	w.Header().Set("Content-Length", strconv.Itoa(len(m.Body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(m.Body)
	}
}

func (f *FlakyRegistry) serveBlob(w http.ResponseWriter, r *http.Request, digest string) {
	f.mu.Lock()
	gen := f.generation
	f.requests++
	f.mu.Unlock()

	data, ok := f.Blobs[digest]
	if !ok {
		http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
		return
	}
	size := int64(len(data))
	start, end, status := int64(0), size, http.StatusOK
	if rh := r.Header.Get("Range"); rh != "" && !f.NoRange {
		s, e, err := parseRange(rh, size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, status = s, e, http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}

	// Decide up front whether, and where, this response fails.
	cut, hang := int64(-1), false
	f.mu.Lock()
	if end > start {
		if x := f.rng.Float64(); x < f.DropRate+f.HangRate {
			cut = start + f.rng.Int63n(end-start)
			hang = x >= f.DropRate
		}
	}
	f.mu.Unlock()

	piece := int64(f.PieceSize)
	if piece <= 0 {
		piece = 16 << 10
	}
	flusher, _ := w.(http.Flusher)
	for off := start; off < end; {
		n := min(piece, end-off)
		if cut >= 0 {
			n = min(n, cut-off)
		}
		if n > 0 {
			if _, err := w.Write(data[off : off+n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			off += n
			f.count(gen, digest, n)
		}
		if cut >= 0 && off >= cut {
			f.mu.Lock()
			if gen == f.generation {
				f.drops++
			}
			f.mu.Unlock()
			if hang {
				<-r.Context().Done() // until the client gives up on us
				return
			}
			panic(http.ErrAbortHandler) // drop the connection
		}
	}
}

func (f *FlakyRegistry) count(gen int, digest string, n int64) {
	f.mu.Lock()
	if gen != f.generation {
		f.mu.Unlock()
		return
	}
	f.served[digest] += n
	total, fn := f.served[digest], f.onServe
	f.mu.Unlock()
	if fn != nil {
		fn(digest, total)
	}
}

// parseRange parses a single "bytes=first-[last]" range into [start, end).
func parseRange(h string, size int64) (start, end int64, err error) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, fmt.Errorf("unsupported range %q", h)
	}
	a, b, _ := strings.Cut(spec, "-")
	if start, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, err
	}
	end = size
	if b != "" {
		last, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		end = min(last+1, size)
	}
	if start < 0 || start >= end {
		return 0, 0, fmt.Errorf("unsatisfiable range %q", h)
	}
	return start, end, nil
}
