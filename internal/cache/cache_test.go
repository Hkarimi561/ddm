package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func TestPutBytesAndHas(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello blob")
	d := digestOf(data)
	if c.Has(d, -1) {
		t.Fatal("empty cache claims to have the blob")
	}
	if err := c.PutBytes(d, data); err != nil {
		t.Fatal(err)
	}
	if !c.Has(d, int64(len(data))) || c.Has(d, 1) {
		t.Fatal("Has does not check presence and size correctly")
	}
	got, err := os.ReadFile(c.BlobPath(d))
	if err != nil || string(got) != string(data) {
		t.Fatalf("stored %q, %v", got, err)
	}
	if err := c.PutBytes(d, []byte("other content")); err == nil {
		t.Fatal("PutBytes accepted content that does not match the digest")
	}
}

func TestPaths(t *testing.T) {
	root := t.TempDir()
	c, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	d := digestOf(nil)
	h := strings.TrimPrefix(d, "sha256:")
	if got, want := c.BlobPath(d), filepath.Join(root, "blobs", "sha256", h); got != want {
		t.Errorf("BlobPath = %s, want %s", got, want)
	}
	p := c.Partial(d)
	for _, path := range []string{p.Data, p.State, p.Lock} {
		if filepath.Dir(path) != filepath.Join(root, "partial") || !strings.Contains(path, h) {
			t.Errorf("unexpected partial path %s", path)
		}
	}
}

func TestParseDigest(t *testing.T) {
	good := digestOf([]byte("x"))
	if _, err := ParseDigest(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "sha256:abc", "md5:" + good[7:], "sha256:" + strings.Repeat("z", 64), good[7:]} {
		if _, err := ParseDigest(bad); err == nil {
			t.Errorf("ParseDigest(%q) accepted a bad digest", bad)
		}
	}
}

func TestDefaultRootHonorsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DDM_CACHE_DIR", dir)
	if got, err := DefaultRoot(); err != nil || got != dir {
		t.Fatalf("DefaultRoot() = %s, %v; want %s", got, err, dir)
	}
}
