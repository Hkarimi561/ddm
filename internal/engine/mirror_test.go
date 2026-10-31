package engine

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Hkarimi561/ddm/internal/testutil"
)

// mirror is a second registry serving the same blob as the fixture.
func (f *fixture) mirror(t *testing.T) (*testutil.FlakyRegistry, Source) {
	t.Helper()
	reg := testutil.NewFlakyRegistry("test", 2)
	reg.AddBlob(f.data)
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	return reg, Source{Name: "mirror", URL: srv.URL + "/v2/test/blobs/" + f.blob.Digest, Client: srv.Client()}
}

type sourceLog struct {
	nopProgress
	mu    sync.Mutex
	names []string
}

func (s *sourceLog) Source(_ string, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.names) == 0 || s.names[len(s.names)-1] != name {
		s.names = append(s.names, name)
	}
}

func TestFallsBackWhenMirrorLacksBlob(t *testing.T) {
	f := newFixture(t, 1<<20)
	empty := testutil.NewFlakyRegistry("test", 3) // has no blobs: answers 404
	srv := httptest.NewServer(empty)
	t.Cleanup(srv.Close)
	missing := Source{Name: "empty-mirror", URL: srv.URL + "/v2/test/blobs/" + f.blob.Digest, Client: srv.Client()}

	log := &sourceLog{}
	d := &Downloader{Cache: f.cache, Options: testOptions(), Progress: log}
	if err := d.Download(context.Background(), f.blob, []Source{missing, f.source()}); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)
	if got := f.reg.TotalServed(); got != f.blob.Size {
		t.Errorf("upstream served %d bytes, want %d", got, f.blob.Size)
	}
	if len(log.names) < 2 || log.names[0] != "empty-mirror" || log.names[len(log.names)-1] != "test" {
		t.Errorf("source sequence = %v", log.names)
	}
}

func TestSwitchesAwayFromFailingMirror(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.reg.FailStatus = 503 // the first source is down
	_, good := f.mirror(t)

	if err := f.downloadFrom(context.Background(), testOptions(), f.source(), good); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)
	if n := f.reg.Requests(); n < testOptions().withDefaults().SwitchAfter {
		t.Errorf("failing source got %d requests; expected at least SwitchAfter before switching", n)
	}
}

func TestEverySourceMissingIsAnError(t *testing.T) {
	f := newFixture(t, 64<<10)
	f.reg.FailStatus = 404
	other, src := f.mirror(t)
	other.FailStatus = 404
	if err := f.downloadFrom(context.Background(), testOptions(), f.source(), src); err == nil {
		t.Fatal("download succeeded although no source has the blob")
	}
}

func TestSlowMirrorIsLeftAndProgressKept(t *testing.T) {
	f := newFixture(t, 4<<20)
	f.reg.Delay = 80 * time.Millisecond // 16 KiB per 80 ms per connection: far below MinSpeed
	fast, fastSrc := f.mirror(t)

	opts := testOptions()
	opts.MinSpeed = 8 << 20
	opts.SlowWindow = time.Second
	if err := f.downloadFrom(context.Background(), opts, f.source(), fastSrc); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)

	slowBytes, fastBytes := f.reg.TotalServed(), fast.TotalServed()
	if slowBytes == 0 {
		t.Fatal("the slow source served nothing, so no switch happened mid-download")
	}
	// Bytes already fetched from the slow source were kept: the fast one
	// only had to supply the rest (plus a little that was in flight).
	if fastBytes >= f.blob.Size {
		t.Errorf("fast mirror served %d bytes of a %d-byte blob; progress from the slow one was thrown away", fastBytes, f.blob.Size)
	}
	t.Logf("slow source %d bytes, fast source %d bytes", slowBytes, fastBytes)
}

func TestSourcesRotationRules(t *testing.T) {
	list := []Source{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	s := newSources(list, 2, nil)
	name := func() string { src, _, _ := s.current(); return src.Name }

	_, gen, ctx := s.current()
	if s.failed(gen) || name() != "a" {
		t.Fatal("switched after one failure")
	}
	if !s.failed(gen) || name() != "b" {
		t.Fatalf("did not switch after two failures (now %s)", name())
	}
	if ctx.Err() == nil {
		t.Fatal("requests on the old source were not canceled")
	}
	if !s.failed(gen) {
		t.Fatal("a stale generation should report that a switch already happened")
	}

	_, gen, _ = s.current()
	if !s.unavailable(gen) || name() != "c" {
		t.Fatalf("unavailable did not move on (now %s)", name())
	}
	if !s.tooSlow() || name() != "a" {
		t.Fatalf("tooSlow did not move to the next live source (now %s)", name())
	}
	// b is dead and c is already known to be slow: nowhere better to go.
	if s.tooSlow() || name() != "a" {
		t.Fatalf("tooSlow switched although every live source is slow (now %s)", name())
	}
	_, gen, _ = s.current()
	if !s.unavailable(gen) || name() != "c" { // a dies; slow-but-alive c is still usable
		t.Fatalf("expected to fall back to c, got %s", name())
	}
	_, gen, _ = s.current()
	if s.unavailable(gen) {
		t.Fatal("reported a live source although all are dead")
	}
}

func (f *fixture) downloadFrom(ctx context.Context, opts Options, srcs ...Source) error {
	return (&Downloader{Cache: f.cache, Options: opts}).Download(ctx, f.blob, srcs)
}
