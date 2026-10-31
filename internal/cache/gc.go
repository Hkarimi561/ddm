package cache

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// GCOptions controls garbage collection.
type GCOptions struct {
	// All removes everything: every ref, blob and partial download not in
	// use by a running process.
	All bool
	// OlderThan also forgets refs not used for this long (0 keeps them).
	OlderThan time.Duration
	// PartialAge removes unfinished downloads untouched for this long
	// (0 keeps them).
	PartialAge time.Duration
	// Grace keeps unreferenced blobs younger than this, because a running
	// pull may have downloaded them and not recorded its ref yet.
	Grace  time.Duration
	DryRun bool
}

// GCResult reports what was (or, in a dry run, would be) removed.
type GCResult struct {
	Refs     []string
	Blobs    int
	Partials int
	Bytes    int64
}

// GC removes blobs no recorded image or model needs, old unfinished
// downloads, and leftovers of interrupted operations. Downloads in progress
// (their lock is held) are never touched.
func (c *Cache) GC(o GCOptions) (GCResult, error) {
	var res GCResult
	refs, err := c.Refs()
	if err != nil {
		return res, err
	}
	keep := map[string]bool{}
	var dropped []string
	for _, r := range refs {
		if o.All || (o.OlderThan > 0 && time.Since(r.Used) > o.OlderThan) {
			dropped = append(dropped, r.Name)
			continue
		}
		for _, b := range r.Blobs {
			keep[b] = true
		}
	}
	res.Refs = dropped
	if len(dropped) > 0 && !o.DryRun {
		drop := map[string]bool{}
		for _, n := range dropped {
			drop[n] = true
		}
		if err := c.updateRefs(func(rs []Ref) []Ref {
			out := rs[:0]
			for _, r := range rs {
				if !drop[r.Name] {
					out = append(out, r)
				}
			}
			return out
		}); err != nil {
			return res, err
		}
	}

	blobs, err := c.Blobs()
	if err != nil {
		return res, err
	}
	for _, b := range blobs {
		if keep[b.Digest] || (!o.All && time.Since(b.ModTime) < o.Grace) {
			continue
		}
		if !o.DryRun {
			if err := os.Remove(c.BlobPath(b.Digest)); err != nil {
				continue
			}
		}
		res.Blobs++
		res.Bytes += b.Size
	}

	partials, err := c.Partials()
	if err != nil {
		return res, err
	}
	for _, p := range partials {
		if !o.All && (o.PartialAge == 0 || time.Since(p.ModTime) < o.PartialAge) {
			continue
		}
		lock := flock.New(p.Paths.Lock)
		ok, err := lock.TryLock()
		if err != nil || !ok {
			continue // being downloaded right now
		}
		if !o.DryRun {
			_ = os.Remove(p.Paths.Data)
			_ = os.Remove(p.Paths.State)
		}
		_ = lock.Unlock()
		res.Partials++
		res.Bytes += p.Size
	}

	if !o.DryRun {
		c.removeLeftovers()
	}
	return res, nil
}

// removeLeftovers deletes temp files and staging directories from
// interrupted operations, and lock files whose download is gone.
func (c *Cache) removeLeftovers() {
	dir := filepath.Join(c.Root, "partial")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name, path := e.Name(), filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		old := time.Since(fi.ModTime()) > time.Hour
		switch {
		case old && (strings.HasPrefix(name, "put-") || strings.HasPrefix(name, "hf-stage-") || strings.HasSuffix(name, ".tmp")):
			_ = os.RemoveAll(path)
		case strings.HasSuffix(name, ".lock"):
			base := strings.TrimSuffix(path, ".lock")
			if _, err := os.Stat(base + ".part"); os.IsNotExist(err) {
				lock := flock.New(path)
				if ok, err := lock.TryLock(); err == nil && ok {
					_ = lock.Unlock()
					_ = os.Remove(path)
				}
			}
		}
	}
}
