// Package engine downloads content-addressed blobs over HTTP using parallel
// Range requests. Progress is persisted continuously, so a download survives
// dropped connections, Ctrl+C, crashes and reboots, and the sha256 digest is
// verified before a blob is committed to the cache.
package engine

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Blob identifies a blob to download.
type Blob struct {
	Digest string // "sha256:<hex>"
	Size   int64
	Name   string // human-readable label for progress output
}

// Source is a place a blob can be fetched from with plain GET requests.
// Redirects (for example to a CDN) are followed by Client.
type Source struct {
	Name   string
	URL    string
	Client *http.Client
}

// Options tunes the downloader. Zero values take the defaults from
// DefaultOptions, except MaxRetries where zero means "retry forever".
type Options struct {
	Connections  int           // parallel connections per blob
	MaxRetries   int           // consecutive failures of one chunk before giving up; 0 = unlimited
	StallTimeout time.Duration // reconnect when no data arrives for this long
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	SaveInterval time.Duration // how often progress is persisted while downloading
	MinChunkSize int64
	MaxChunkSize int64
}

// DefaultOptions returns the default tuning.
func DefaultOptions() Options {
	return Options{
		Connections:  4,
		StallTimeout: 30 * time.Second,
		BackoffBase:  time.Second,
		BackoffMax:   30 * time.Second,
		SaveInterval: time.Second,
		MinChunkSize: 1 << 20,
		MaxChunkSize: 32 << 20,
	}
}

func (o Options) withDefaults() Options {
	def := DefaultOptions()
	if o.Connections < 1 {
		o.Connections = def.Connections
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = def.StallTimeout
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = def.BackoffBase
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = def.BackoffMax
	}
	if o.SaveInterval <= 0 {
		o.SaveInterval = def.SaveInterval
	}
	if o.MinChunkSize <= 0 {
		o.MinChunkSize = def.MinChunkSize
	}
	if o.MaxChunkSize <= 0 {
		o.MaxChunkSize = def.MaxChunkSize
	}
	if o.MaxChunkSize < o.MinChunkSize {
		o.MaxChunkSize = o.MinChunkSize
	}
	return o
}

// Progress receives download events. Implementations must be safe for
// concurrent use; blobs are identified by digest.
type Progress interface {
	// Start (re)starts tracking b; current is the number of bytes already present.
	Start(b Blob, current int64)
	Add(digest string, n int64)
	// Set replaces the byte count, e.g. after data had to be discarded.
	Set(digest string, current int64)
	Retry(digest string, attempt int, err error)
	Done(digest string)
	Fail(digest string, err error)
}

type nopProgress struct{}

func (nopProgress) Start(Blob, int64)        {}
func (nopProgress) Add(string, int64)        {}
func (nopProgress) Set(string, int64)        {}
func (nopProgress) Retry(string, int, error) {}
func (nopProgress) Done(string)              {}
func (nopProgress) Fail(string, error)       {}

// ShortDigest returns the first 12 hex characters of a digest, as Docker shows them.
func ShortDigest(d string) string {
	_, h, _ := strings.Cut(d, ":")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
