// Package cache implements ddm's content-addressed blob store.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cache is a content-addressed blob store rooted at a directory:
//
//	<root>/blobs/sha256/<hex>          complete, verified blobs
//	<root>/partial/sha256-<hex>.part   data of an in-progress download
//	<root>/partial/sha256-<hex>.json   persisted progress of that download
//	<root>/partial/sha256-<hex>.lock   inter-process lock for that download
type Cache struct {
	Root string
}

// Partial holds the paths used while a blob is being downloaded.
type Partial struct {
	Data  string
	State string
	Lock  string
}

// DefaultRoot returns $DDM_CACHE_DIR, or "ddm" inside the OS cache directory
// (~/.cache on Linux, ~/Library/Caches on macOS, %LocalAppData% on Windows).
func DefaultRoot() (string, error) {
	if d := os.Getenv("DDM_CACHE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ddm"), nil
}

// New opens (creating if needed) the cache rooted at root.
func New(root string) (*Cache, error) {
	for _, dir := range []string{filepath.Join(root, "blobs", "sha256"), filepath.Join(root, "partial")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Cache{Root: root}, nil
}

// ParseDigest validates a "sha256:<hex>" digest and returns its hex part.
func ParseDigest(d string) (string, error) {
	algo, h, ok := strings.Cut(d, ":")
	if !ok || algo != "sha256" || len(h) != 64 {
		return "", fmt.Errorf("unsupported digest %q (only sha256 is supported)", d)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("malformed digest %q", d)
	}
	return h, nil
}

func hexOf(d string) string { return strings.TrimPrefix(d, "sha256:") }

// BlobPath returns where the complete blob with digest d is stored.
func (c *Cache) BlobPath(d string) string {
	return filepath.Join(c.Root, "blobs", "sha256", hexOf(d))
}

// Partial returns the paths used while downloading the blob with digest d.
func (c *Cache) Partial(d string) Partial {
	base := filepath.Join(c.Root, "partial", "sha256-"+hexOf(d))
	return Partial{Data: base + ".part", State: base + ".json", Lock: base + ".lock"}
}

// Has reports whether the complete blob is cached. A negative size skips the
// size check.
func (c *Cache) Has(d string, size int64) bool {
	fi, err := os.Stat(c.BlobPath(d))
	return err == nil && fi.Mode().IsRegular() && (size < 0 || fi.Size() == size)
}

// Open opens a cached blob for reading.
func (c *Cache) Open(d string) (*os.File, error) {
	return os.Open(c.BlobPath(d))
}

// Commit moves an already verified file into the store as blob d.
func (c *Cache) Commit(src, d string) error {
	dst := c.BlobPath(d)
	var err error
	// On Windows, virus scanners and the indexer can hold a freshly written
	// file for a moment, making the rename fail transiently.
	for range 10 {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// PutBytes verifies data against digest d and stores it.
func (c *Cache) PutBytes(d string, data []byte) error {
	want, err := ParseDigest(d)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("content does not match digest %s", d)
	}
	if c.Has(d, int64(len(data))) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Join(c.Root, "partial"), "put-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = c.Commit(name, d)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
