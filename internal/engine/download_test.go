package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/testutil"
)

const testChunk = 256 << 10

func testOptions() Options {
	return Options{
		Connections:  4,
		StallTimeout: 5 * time.Second,
		BackoffBase:  time.Millisecond,
		BackoffMax:   20 * time.Millisecond,
		SaveInterval: 10 * time.Millisecond,
		MinChunkSize: testChunk,
		MaxChunkSize: testChunk,
	}
}

type fixture struct {
	reg   *testutil.FlakyRegistry
	srv   *httptest.Server
	cache *cache.Cache
	data  []byte
	blob  Blob
}

func newFixture(t *testing.T, size int) *fixture {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	reg := testutil.NewFlakyRegistry("test", 1)
	dgst := reg.AddBlob(data)
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	c, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{reg: reg, srv: srv, cache: c, data: data, blob: Blob{Digest: dgst, Size: int64(size), Name: "test"}}
}

func (f *fixture) source() Source {
	return Source{Name: "test", URL: f.srv.URL + "/v2/test/blobs/" + f.blob.Digest, Client: f.srv.Client()}
}

func (f *fixture) download(ctx context.Context, opts Options) error {
	return (&Downloader{Cache: f.cache, Options: opts}).Download(ctx, f.blob, []Source{f.source()})
}

func (f *fixture) assertCached(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.cache.BlobPath(f.blob.Digest))
	if err != nil {
		t.Fatalf("blob not in cache: %v", err)
	}
	if !bytes.Equal(got, f.data) {
		t.Fatal("cached blob differs from the original")
	}
	if _, err := os.Stat(f.cache.Partial(f.blob.Digest).State); !os.IsNotExist(err) {
		t.Errorf("download state left behind (stat err: %v)", err)
	}
}

func TestDownloadSurvivesDroppedConnections(t *testing.T) {
	f := newFixture(t, 3<<20)
	f.reg.DropRate = 0.6
	f.reg.Redirect = true

	if err := f.download(context.Background(), testOptions()); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)

	drops := f.reg.Drops()
	if drops == 0 {
		t.Fatal("the server never dropped a connection, so nothing was tested")
	}
	// Bytes received before a drop are kept, so the blob crosses the wire
	// about once. Allow one piece per drop for data lost in flight.
	served := f.reg.TotalServed()
	if limit := f.blob.Size + int64(drops)*(16<<10); served > limit {
		t.Errorf("served %d bytes for a %d-byte blob with %d drops; data was re-downloaded", served, f.blob.Size, drops)
	}
	t.Logf("%d drops, %d bytes served for a %d-byte blob", drops, served, f.blob.Size)
}

func TestStalledConnectionsAreRetried(t *testing.T) {
	f := newFixture(t, 1<<20)
	f.reg.HangRate = 0.5
	opts := testOptions()
	opts.StallTimeout = 200 * time.Millisecond

	if err := f.download(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)
	if f.reg.Drops() == 0 {
		t.Fatal("the server never hung, so nothing was tested")
	}
}

func TestResumeAfterInterrupt(t *testing.T) {
	f := newFixture(t, 4<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	half := f.blob.Size / 2
	f.reg.OnServe(func(_ string, served int64) {
		if served >= half {
			cancel() // like Ctrl+C half-way through
		}
	})

	if err := f.download(ctx, testOptions()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	st, err := loadState(f.cache.Partial(f.blob.Digest).State)
	if err != nil {
		t.Fatalf("no saved state after the interrupt: %v", err)
	}
	var saved int64
	for _, c := range st.Chunks {
		saved += c.Done
	}
	if saved == 0 || saved >= f.blob.Size {
		t.Fatalf("saved progress is %d of %d bytes; want a partial download", saved, f.blob.Size)
	}

	// A new downloader (as after a restart) fetches exactly what is missing.
	f.reg.OnServe(nil)
	f.reg.Reset()
	if err := f.download(context.Background(), testOptions()); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)
	if served, want := f.reg.TotalServed(), f.blob.Size-saved; served != want {
		t.Errorf("resumed download fetched %d bytes; want exactly the %d missing ones", served, want)
	}
}

func TestCorruptedChunkIsRedownloadedAlone(t *testing.T) {
	f := newFixture(t, 2<<20)
	d := &Downloader{Cache: f.cache, Options: testOptions()}
	const off = 3*testChunk + 100
	corrupted := false
	d.beforeVerify = func(path string) {
		if corrupted {
			return
		}
		corrupted = true
		fh, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer fh.Close()
		if _, err := fh.WriteAt([]byte{^f.data[off]}, off); err != nil {
			t.Error(err)
		}
	}

	if err := d.Download(context.Background(), f.blob, []Source{f.source()}); err != nil {
		t.Fatal(err)
	}
	f.assertCached(t)
	if served, want := f.reg.TotalServed(), f.blob.Size+testChunk; served != want {
		t.Errorf("served %d bytes; want %d (the blob once plus the corrupted chunk)", served, want)
	}
}

func TestFallsBackWhenRangeIsUnsupported(t *testing.T) {
	for _, dropRate := range []float64{0, 0.3} {
		f := newFixture(t, 1<<20)
		f.reg.NoRange = true
		f.reg.DropRate = dropRate
		if err := f.download(context.Background(), testOptions()); err != nil {
			t.Fatalf("drop rate %v: %v", dropRate, err)
		}
		f.assertCached(t)
	}
}

func TestDigestMismatchIsReported(t *testing.T) {
	f := newFixture(t, 512<<10)
	bad := bytes.Clone(f.data)
	bad[1000] ^= 0xff
	f.reg.Blobs[f.blob.Digest] = bad

	err := f.download(context.Background(), testOptions())
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
	if f.cache.Has(f.blob.Digest, -1) {
		t.Fatal("a blob with the wrong digest was committed to the cache")
	}
}

func TestMissingBlobFailsWithoutRetrying(t *testing.T) {
	f := newFixture(t, 1024)
	f.blob.Digest = testutil.Digest([]byte("not on the server"))

	if err := f.download(context.Background(), testOptions()); err == nil {
		t.Fatal("download of a missing blob succeeded")
	}
	if n := f.reg.Requests(); n != 1 {
		t.Errorf("made %d requests for a 404 blob; want 1", n)
	}
}

func TestCachedBlobIsNotDownloaded(t *testing.T) {
	f := newFixture(t, 64<<10)
	if err := f.cache.PutBytes(f.blob.Digest, f.data); err != nil {
		t.Fatal(err)
	}
	if err := f.download(context.Background(), testOptions()); err != nil {
		t.Fatal(err)
	}
	if n := f.reg.Requests(); n != 0 {
		t.Errorf("made %d requests for a cached blob", n)
	}
}
