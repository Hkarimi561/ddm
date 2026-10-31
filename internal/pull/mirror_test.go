package pull

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/Hkarimi561/ddm/internal/testutil"
)

// A Harbor-style mirror that serves the upstream repository under a
// "proxy/" project, has the manifest, but is missing one layer.
func TestPullThroughMirrorWithFallback(t *testing.T) {
	img, err := random.Image(128<<10, 3)
	if err != nil {
		t.Fatal(err)
	}
	up, ref := serveImage(t, img)

	layers, _ := img.Layers()
	missingDigest, _ := layers[0].Digest()
	missing := missingDigest.String()

	mirror := testutil.NewFlakyRegistry("proxy/test/app", 9)
	for k, v := range up.Manifests {
		mirror.Manifests[k] = v
	}
	for d, b := range up.Blobs {
		if d != missing {
			mirror.Blobs[d] = b
		}
	}
	msrv := httptest.NewServer(mirror)
	t.Cleanup(msrv.Close)
	mirrorURL := msrv.URL + "/proxy"

	p, opts := newTestPuller(t)
	p.Registry.Mirrors = map[string][]string{ref.Context().RegistryStr(): {mirrorURL}}
	ctx := context.Background()

	ri, err := p.Registry.ResolveImage(ctx, ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if ri.Source != mirrorURL {
		t.Errorf("manifest came from %q, want the mirror %q", ri.Source, mirrorURL)
	}
	if err := p.Fetch(ctx, ri, opts); err != nil {
		t.Fatal(err)
	}
	for _, b := range blobsOf(ri) {
		if !p.Cache.Has(b.Digest, b.Size) {
			t.Errorf("%s missing from the cache", b.Name)
		}
	}
	// Only the layer the mirror lacks may come from upstream.
	if got, want := up.TotalServed(), int64(len(up.Blobs[missing])); got != want {
		t.Errorf("upstream served %d bytes, want only the missing layer's %d", got, want)
	}
	if mirror.TotalServed() == 0 {
		t.Error("the mirror served nothing")
	}
}

func TestManifestFallsBackToUpstream(t *testing.T) {
	img, err := random.Image(16<<10, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, ref := serveImage(t, img)
	empty := httptest.NewServer(testutil.NewFlakyRegistry("test/app", 4)) // knows no manifests
	t.Cleanup(empty.Close)

	p, _ := newTestPuller(t)
	p.Registry.Mirrors = map[string][]string{ref.Context().RegistryStr(): {empty.URL}}
	ri, err := p.Registry.ResolveImage(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if ri.Source != ref.Context().RegistryStr() {
		t.Errorf("manifest came from %q, want the upstream", ri.Source)
	}
}

func TestEndpointsOrder(t *testing.T) {
	p, _ := newTestPuller(t)
	img, _ := random.Image(16, 1)
	_, ref := serveImage(t, img)
	reg := ref.Context().Registry
	upstream := "http://" + reg.RegistryStr()

	p.Registry.Mirrors = map[string][]string{reg.RegistryStr(): {"https://a.example", upstream, "https://b.example/v2/hub"}}
	eps := p.Registry.Endpoints(reg)
	var names []string
	for _, e := range eps {
		names = append(names, e.Name)
	}
	if want := []string{"https://a.example", reg.RegistryStr(), "https://b.example/v2/hub"}; strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("endpoints = %v, want %v (upstream keeps its configured place)", names, want)
	}
	if got := eps[2].Repo(ref.Context()).String(); got != "b.example/hub/test/app" {
		t.Errorf("prefixed mirror repo = %s", got)
	}

	p.Registry.Mirrors = nil
	if eps := p.Registry.Endpoints(reg); len(eps) != 1 || !eps[0].Upstream {
		t.Fatalf("without mirrors, want only the upstream, got %+v", eps)
	}
}
