package pull

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/registry"
	"github.com/Hkarimi561/ddm/internal/testutil"
)

// serveImage publishes img as test/app:v1 on a flaky registry.
func serveImage(t *testing.T, img v1.Image) (*testutil.FlakyRegistry, name.Reference) {
	t.Helper()
	reg := testutil.NewFlakyRegistry("test/app", 7)
	raw, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	mt, err := img.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	reg.Manifests["v1"] = testutil.Manifest{MediaType: string(mt), Body: raw}
	cfg, err := img.RawConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	reg.AddBlob(cfg)
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		reg.AddBlob(b)
	}

	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/test/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	return reg, ref
}

func newTestPuller(t *testing.T) (*Puller, Options) {
	t.Helper()
	c, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rc := &registry.Client{Keychain: authn.NewMultiKeychain(), Transport: registry.NewTransport(nil), MaxRetries: 3, Log: log}
	p := &Puller{Cache: c, Registry: rc, Out: io.Discard, Log: log}
	opts := Options{ParallelBlobs: 2, Engine: engine.Options{
		BackoffBase:  time.Millisecond,
		BackoffMax:   10 * time.Millisecond,
		SaveInterval: 10 * time.Millisecond,
		MinChunkSize: 64 << 10,
		MaxChunkSize: 64 << 10,
	}}
	return p, opts
}

func TestFetchFromFlakyRegistryAndBuildArchive(t *testing.T) {
	img, err := random.Image(256<<10, 4)
	if err != nil {
		t.Fatal(err)
	}
	reg, ref := serveImage(t, img)
	reg.DropRate = 0.5
	p, opts := newTestPuller(t)
	ctx := context.Background()

	ri, err := p.Registry.ResolveImage(ctx, ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Fetch(ctx, ri, opts); err != nil {
		t.Fatal(err)
	}
	if reg.Drops() == 0 {
		t.Error("the registry never dropped a connection, so resumption was not exercised")
	}
	for _, b := range blobsOf(ri) {
		if !p.Cache.Has(b.Digest, b.Size) {
			t.Errorf("%s (%s) missing from the cache", b.Name, b.Digest)
		}
	}

	// Everything is cached now: fetching again must not download anything.
	reg.Reset()
	if err := p.Fetch(ctx, ri, opts); err != nil {
		t.Fatal(err)
	}
	if n := reg.TotalServed(); n != 0 {
		t.Errorf("second fetch downloaded %d bytes; want 0", n)
	}

	// The archive must be readable as a docker-archive with the original
	// config and layers...
	var buf bytes.Buffer
	if err := docker.WriteArchive(&buf, p.archive(ri)); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	loaded, err := tarball.Image(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	}, nil)
	if err != nil {
		t.Fatalf("archive is not a valid docker-archive: %v", err)
	}
	wantCfg, _ := img.ConfigName()
	if gotCfg, err := loaded.ConfigName(); err != nil || gotCfg != wantCfg {
		t.Errorf("config digest = %v (%v), want %v", gotCfg, err, wantCfg)
	}
	wantLayers, _ := img.Layers()
	gotLayers, err := loaded.Layers()
	if err != nil || len(gotLayers) != len(wantLayers) {
		t.Fatalf("archive has %d layers (%v), want %d", len(gotLayers), err, len(wantLayers))
	}
	for i := range gotLayers {
		got, _ := gotLayers[i].Digest()
		want, _ := wantLayers[i].Digest()
		if got != want {
			t.Errorf("layer %d digest = %v, want %v", i, got, want)
		}
	}

	// ...and as an OCI layout that keeps the original manifest and name.
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(tarFile(t, archive, "index.json"), &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != ri.ManifestDigest {
		t.Fatalf("index.json = %+v, want the manifest %s", index, ri.ManifestDigest)
	}
	if got := index.Manifests[0].Annotations["io.containerd.image.name"]; !strings.HasSuffix(got, "/test/app:v1") {
		t.Errorf("image name annotation = %q", got)
	}
	if got := tarFile(t, archive, "blobs/sha256/"+strings.TrimPrefix(ri.ManifestDigest, "sha256:")); !bytes.Equal(got, ri.RawManifest) {
		t.Error("archive does not contain the original manifest bytes")
	}
}

func TestArchiveRejectsCorruptCachedBlob(t *testing.T) {
	img, err := random.Image(64<<10, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, ref := serveImage(t, img)
	p, opts := newTestPuller(t)
	ctx := context.Background()
	ri, err := p.Registry.ResolveImage(ctx, ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Fetch(ctx, ri, opts); err != nil {
		t.Fatal(err)
	}
	layer := ri.Layers[0].Digest
	if err := os.WriteFile(p.Cache.BlobPath(layer), []byte("bit rot"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = docker.WriteArchive(io.Discard, p.archive(ri))
	var ce *docker.CorruptBlobError
	if !errors.As(err, &ce) || ce.Digest != layer {
		t.Fatalf("got %v, want a CorruptBlobError for %s", err, layer)
	}
}

func tarFile(t *testing.T, archive []byte, path string) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatalf("%s not found in archive: %v", path, err)
		}
		if hdr.Name == path {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
}
