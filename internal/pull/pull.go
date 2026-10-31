// Package pull implements `ddm pull`: resolve an image, download its blobs
// with the resumable engine, and load the result into Docker.
package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"golang.org/x/sync/errgroup"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/diskspace"
	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/registry"
	"github.com/Hkarimi561/ddm/internal/ui"
)

// Reporter shows download progress.
type Reporter interface {
	engine.Progress
	Close()
}

// Options controls a pull.
type Options struct {
	Platform      string // e.g. "linux/arm64"; empty means the Docker daemon's platform
	ParallelBlobs int    // blobs downloaded at the same time
	NoLoad        bool   // download into the cache only
	Engine        engine.Options
}

// Puller pulls images.
type Puller struct {
	Cache       *cache.Cache
	Registry    *registry.Client
	Docker      *docker.CLI // nil: download only
	NewReporter func(total int64) Reporter
	Out         io.Writer
	Log         *slog.Logger
}

// Pull downloads ref and, unless opts.NoLoad is set, loads it into Docker
// under its original name.
func (p *Puller) Pull(ctx context.Context, refStr string, opts Options) error {
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return fmt.Errorf("invalid image reference %q: %w", refStr, err)
	}
	load := p.Docker != nil && !opts.NoLoad
	platform, err := p.platform(ctx, opts.Platform, load)
	if err != nil {
		return err
	}

	fmt.Fprintf(p.Out, "Resolving %s (%s)\n", docker.FamiliarName(ref), platform.String())
	img, err := p.Registry.ResolveImage(ctx, ref, platform)
	if err != nil {
		return err
	}
	if img.Source != p.Registry.Endpoints(ref.Context().Registry)[0].Name {
		fmt.Fprintf(p.Out, "Manifest fetched from %s\n", img.Source)
	}
	if load {
		if done, err := p.alreadyInDocker(ctx, img); err != nil || done {
			return err
		}
	}
	if err := p.Fetch(ctx, img, opts); err != nil {
		return err
	}
	if !load {
		fmt.Fprintf(p.Out, "Downloaded %s into the ddm cache (not loaded into Docker).\n", docker.FamiliarName(ref))
		return nil
	}
	return p.Import(ctx, img)
}

func (p *Puller) platform(ctx context.Context, s string, fromDocker bool) (v1.Platform, error) {
	if s == "" && fromDocker {
		var err error
		if s, err = p.Docker.ServerPlatform(ctx); err != nil {
			return v1.Platform{}, err
		}
	}
	if s == "" {
		s = "linux/" + runtime.GOARCH
	}
	pl, err := v1.ParsePlatform(s)
	if err != nil {
		return v1.Platform{}, fmt.Errorf("invalid platform %q: %w", s, err)
	}
	return *pl, nil
}

// alreadyInDocker reports whether Docker already has img, tagging it under
// the requested name if it only exists under another one.
func (p *Puller) alreadyInDocker(ctx context.Context, img *registry.Image) (bool, error) {
	fam := docker.FamiliarName(img.Ref)
	id, err := p.Docker.ImageID(ctx, fam)
	if err != nil {
		return false, err
	}
	// The classic image store identifies images by config digest; the
	// containerd store by manifest (or index) digest.
	matches := func(id string) bool {
		return id != "" && (id == img.Config.Digest || id == img.ManifestDigest || id == img.IndexDigest)
	}
	if matches(id) {
		fmt.Fprintf(p.Out, "%s is up to date.\n", fam)
		return true, nil
	}
	if _, isTag := img.Ref.(name.Tag); !isTag {
		return false, nil
	}
	if id, err := p.Docker.ImageID(ctx, img.Config.Digest); err == nil && matches(id) {
		if err := p.Docker.Tag(ctx, id, fam); err != nil {
			return false, err
		}
		fmt.Fprintf(p.Out, "Docker already has this image; tagged it as %s.\n", fam)
		return true, nil
	}
	return false, nil
}

// Fetch makes sure the manifest, config and every layer of img are in the
// cache, downloading whatever is missing.
func (p *Puller) Fetch(ctx context.Context, img *registry.Image, opts Options) error {
	if err := p.Cache.PutBytes(img.ManifestDigest, img.RawManifest); err != nil {
		return fmt.Errorf("caching manifest: %w", err)
	}
	all := blobsOf(img)
	var missing []engine.Blob
	var total int64
	for _, b := range all {
		if !p.Cache.Has(b.Digest, b.Size) {
			missing = append(missing, b)
			total += b.Size
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := p.CheckSpace(total); err != nil {
		return err
	}

	eps, err := p.Registry.BlobEndpoints(ctx, img.Ref.Context())
	if err != nil {
		return err
	}
	fmt.Fprintf(p.Out, "Downloading %d of %d blobs (%s)\n", len(missing), len(all), ui.HumanBytes(total))
	if len(eps) > 1 {
		names := make([]string, len(eps))
		for i, e := range eps {
			names[i] = e.Name
		}
		fmt.Fprintf(p.Out, "Sources, in order: %s\n", strings.Join(names, ", "))
	}

	var rep Reporter = nopReporter{}
	if p.NewReporter != nil {
		rep = p.NewReporter(total)
	}
	dl := &engine.Downloader{Cache: p.Cache, Options: opts.Engine, Progress: rep, Log: p.Log}
	parallel := opts.ParallelBlobs
	if parallel < 1 {
		parallel = 3
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for _, b := range missing {
		g.Go(func() error { return dl.Download(gctx, b, registry.Sources(eps, b.Digest)) })
	}
	err = g.Wait()
	rep.Close()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// NoSpaceError means the cache's disk can't hold a download.
type NoSpaceError struct {
	Path       string
	Need, Free uint64
}

func (e *NoSpaceError) Error() string {
	return fmt.Sprintf("not enough disk space: the download needs %s but only %s is free at %s; free up space or move the cache with `ddm config set cache_dir <dir>`",
		ui.HumanBytes(int64(e.Need)), ui.HumanBytes(int64(e.Free)), e.Path)
}

// spaceMargin is kept free beyond the download itself.
const spaceMargin = 256 << 20

// CheckSpace makes sure the cache's disk can hold another n bytes. Partial
// downloads already have their full size reserved, but counting them again
// errs on the safe side. If free space can't be determined, it doesn't block.
func (p *Puller) CheckSpace(n int64) error {
	free, err := diskspace.Free(p.Cache.Root)
	if err != nil {
		if p.Log != nil {
			p.Log.Debug("could not determine free disk space", "err", err)
		}
		return nil
	}
	need := uint64(n) + spaceMargin
	if free < need {
		return &NoSpaceError{Path: p.Cache.Root, Need: need, Free: free}
	}
	return nil
}

// Import loads a fully cached img into Docker.
func (p *Puller) Import(ctx context.Context, img *registry.Image) error {
	fmt.Fprintf(p.Out, "Loading %s into Docker\n", docker.FamiliarName(img.Ref))
	a := p.archive(img)
	out, err := p.Docker.Load(ctx, func(w io.Writer) error { return docker.WriteArchive(w, a) })
	if err != nil {
		var ce *docker.CorruptBlobError
		if errors.As(err, &ce) {
			_ = os.Remove(p.Cache.BlobPath(ce.Digest))
			return fmt.Errorf("%w; it was removed from the cache, run the command again to download it again", err)
		}
		return err
	}
	if out != "" {
		fmt.Fprintln(p.Out, out)
	}
	return nil
}

func (p *Puller) archive(img *registry.Image) docker.Archive {
	a := docker.Archive{
		Name:              docker.QualifiedName(img.Ref),
		ManifestDigest:    img.ManifestDigest,
		ManifestMediaType: img.ManifestMediaType,
		RawManifest:       img.RawManifest,
		Config:            img.Config.Digest,
		Open:              p.Cache.Open,
	}
	if t, ok := img.Ref.(name.Tag); ok {
		a.RepoTag = docker.FamiliarName(t)
		a.RefName = t.TagStr()
	}
	for _, l := range img.Layers {
		a.Layers = append(a.Layers, l.Digest)
	}
	return a
}

// blobsOf lists the config and layers of img, without duplicates.
func blobsOf(img *registry.Image) []engine.Blob {
	seen := map[string]bool{}
	var out []engine.Blob
	add := func(d registry.Descriptor, label string) {
		if seen[d.Digest] {
			return
		}
		seen[d.Digest] = true
		out = append(out, engine.Blob{Digest: d.Digest, Size: d.Size, Name: label})
	}
	add(img.Config, "config")
	for i, l := range img.Layers {
		add(l, fmt.Sprintf("layer %d/%d %s", i+1, len(img.Layers), engine.ShortDigest(l.Digest)))
	}
	return out
}

type nopReporter struct{}

func (nopReporter) Start(engine.Blob, int64)  {}
func (nopReporter) Add(string, int64)        {}
func (nopReporter) Set(string, int64)        {}
func (nopReporter) Retry(string, int, error) {}
func (nopReporter) Source(string, string)    {}
func (nopReporter) Done(string)              {}
func (nopReporter) Fail(string, error)       {}
func (nopReporter) Close()                   {}
