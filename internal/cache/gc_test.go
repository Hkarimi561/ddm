package cache

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func putBlob(t *testing.T, c *Cache, content string, age time.Duration) string {
	t.Helper()
	d := digestOf([]byte(content))
	if err := c.PutBytes(d, []byte(content)); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(c.BlobPath(d), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestRefsRememberAndForget(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Remember(Ref{Name: "nginx", Kind: "image", Blobs: []string{"sha256:a"}, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Remember(Ref{Name: "ai/smollm2:latest", Kind: "model", Size: 2}); err != nil {
		t.Fatal(err)
	}
	if err := c.Remember(Ref{Name: "nginx", Kind: "image", Size: 3}); err != nil { // refresh, not duplicate
		t.Fatal(err)
	}
	refs, err := c.Refs()
	if err != nil || len(refs) != 2 || refs[0].Name != "nginx" || refs[0].Size != 3 {
		t.Fatalf("refs = %+v, %v", refs, err)
	}
	if ok, err := c.Forget("nginx"); !ok || err != nil {
		t.Fatalf("Forget = %v, %v", ok, err)
	}
	if ok, _ := c.Forget("nginx"); ok {
		t.Error("forgot the same ref twice")
	}
	if refs, _ := c.Refs(); len(refs) != 1 {
		t.Errorf("refs after Forget = %+v", refs)
	}
}

func TestGC(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kept := putBlob(t, c, "kept layer", 48*time.Hour)
	orphan := putBlob(t, c, "orphan layer", 48*time.Hour)
	fresh := putBlob(t, c, "just downloaded", 0) // unreferenced but within the grace period
	oldRef := putBlob(t, c, "old image layer", 48*time.Hour)

	if err := c.Remember(Ref{Name: "app:1", Kind: "image", Blobs: []string{kept}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Remember(Ref{Name: "app:0", Kind: "image", Blobs: []string{oldRef}, Used: time.Now().Add(-90 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// An abandoned partial download, and one in progress (lock held).
	abandoned := c.Partial(digestOf([]byte("abandoned")))
	active := c.Partial(digestOf([]byte("active")))
	for _, p := range []Partial{abandoned, active} {
		if err := os.WriteFile(p.Data, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-30 * 24 * time.Hour)
		_ = os.Chtimes(p.Data, old, old)
	}
	lock := flock.New(active.Lock)
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal("could not take the active download's lock")
	}
	defer lock.Unlock()

	dry, err := c.GC(GCOptions{OlderThan: 30 * 24 * time.Hour, PartialAge: 7 * 24 * time.Hour, Grace: time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Blobs != 2 || dry.Partials != 1 || len(dry.Refs) != 1 || dry.Refs[0] != "app:0" {
		t.Fatalf("dry run = %+v", dry)
	}
	if !c.Has(orphan, -1) {
		t.Fatal("a dry run removed something")
	}

	res, err := c.GC(GCOptions{OlderThan: 30 * 24 * time.Hour, PartialAge: 7 * 24 * time.Hour, Grace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != dry.Blobs || res.Partials != dry.Partials {
		t.Errorf("gc = %+v, dry run said %+v", res, dry)
	}
	for d, want := range map[string]bool{kept: true, fresh: true, orphan: false, oldRef: false} {
		if got := c.Has(d, -1); got != want {
			t.Errorf("blob %s present = %v, want %v", d[:15], got, want)
		}
	}
	if _, err := os.Stat(abandoned.Data); !os.IsNotExist(err) {
		t.Error("the abandoned partial download was kept")
	}
	if _, err := os.Stat(active.Data); err != nil {
		t.Error("the partial download in progress was removed")
	}
	if refs, _ := c.Refs(); len(refs) != 1 || refs[0].Name != "app:1" {
		t.Errorf("refs after gc = %+v", refs)
	}

	all, err := c.GC(GCOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if all.Blobs != 2 || c.Has(kept, -1) || c.Has(fresh, -1) {
		t.Errorf("--all left blobs behind: %+v", all)
	}
	if !strings.HasPrefix(active.Data, c.Root) {
		t.Fatal("bad partial path")
	}
	if _, err := os.Stat(active.Data); err != nil {
		t.Error("--all removed a download in progress")
	}
}
