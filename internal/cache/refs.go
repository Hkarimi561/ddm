package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
)

// Ref records which blobs make up something ddm downloaded, so `ddm cache`
// can list it and gc knows what is still needed.
type Ref struct {
	Name  string    `json:"name"` // e.g. docker.io/library/nginx:latest, huggingface.co/org/repo:Q4_K_M
	Kind  string    `json:"kind"` // "image" or "model"
	Blobs []string  `json:"blobs"`
	Size  int64     `json:"size"`
	Used  time.Time `json:"used"`
}

func (c *Cache) refsPath() string { return filepath.Join(c.Root, "refs.json") }

// updateRefs changes the index under an inter-process lock.
func (c *Cache) updateRefs(fn func([]Ref) []Ref) error {
	lock := flock.New(filepath.Join(c.Root, "refs.lock"))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	refs, err := c.Refs()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(fn(refs), "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.Root, "refs-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.refsPath())
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// Remember records (or refreshes) ref.
func (c *Cache) Remember(ref Ref) error {
	if ref.Used.IsZero() {
		ref.Used = time.Now()
	}
	return c.updateRefs(func(refs []Ref) []Ref {
		for i := range refs {
			if refs[i].Name == ref.Name && refs[i].Kind == ref.Kind {
				refs[i] = ref
				return refs
			}
		}
		return append(refs, ref)
	})
}

// Forget removes the refs with this name and reports whether there were any.
// Their blobs go away at the next gc unless something else uses them.
func (c *Cache) Forget(name string) (bool, error) {
	found := false
	err := c.updateRefs(func(refs []Ref) []Ref {
		out := refs[:0]
		for _, r := range refs {
			if r.Name == name {
				found = true
				continue
			}
			out = append(out, r)
		}
		return out
	})
	return found, err
}

// Refs returns the recorded refs, most recently used first.
func (c *Cache) Refs() ([]Ref, error) {
	b, err := os.ReadFile(c.refsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []Ref
	if err := json.Unmarshal(b, &refs); err != nil {
		return nil, err
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Used.After(refs[j].Used) })
	return refs, nil
}

// BlobInfo describes a complete blob in the cache.
type BlobInfo struct {
	Digest  string
	Size    int64
	ModTime time.Time
}

// Blobs lists the complete blobs.
func (c *Cache) Blobs() ([]BlobInfo, error) {
	entries, err := os.ReadDir(filepath.Join(c.Root, "blobs", "sha256"))
	if err != nil {
		return nil, err
	}
	var out []BlobInfo
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, BlobInfo{Digest: "sha256:" + e.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	return out, nil
}

// PartialInfo describes an unfinished download.
type PartialInfo struct {
	Digest  string
	Paths   Partial
	Size    int64 // size of the data file (the blob's full size, sparse)
	ModTime time.Time
}

// Partials lists unfinished downloads.
func (c *Cache) Partials() ([]PartialInfo, error) {
	entries, err := os.ReadDir(filepath.Join(c.Root, "partial"))
	if err != nil {
		return nil, err
	}
	var out []PartialInfo
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".part" || len(name) < len("sha256-")+64 {
			continue
		}
		d := "sha256:" + name[len("sha256-"):len(name)-len(".part")]
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, PartialInfo{Digest: d, Paths: c.Partial(d), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	return out, nil
}
