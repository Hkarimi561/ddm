// Package modelpull implements `ddm model pull`: download a Docker model
// (from a registry, or the Hugging Face Hub) with the resumable engine and
// load it into Docker Model Runner under the name `docker model` uses.
// See docs/model-runner-research.md for why it works this way.
package modelpull

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/hf"
	"github.com/Hkarimi561/ddm/internal/modelref"
	"github.com/Hkarimi561/ddm/internal/modelrunner"
	"github.com/Hkarimi561/ddm/internal/pull"
	"github.com/Hkarimi561/ddm/internal/registry"
	"github.com/Hkarimi561/ddm/internal/sysmem"
	"github.com/Hkarimi561/ddm/internal/ui"
)

// hfRevision is the branch models are taken from, as the runner does.
const hfRevision = "main"

// Puller pulls models.
type Puller struct {
	Images *pull.Puller // cache, registry client, progress output
	Opts   pull.Options
	Hub    *hf.Hub
	Runner *modelrunner.Client // nil: download into the cache only
	Docker *docker.CLI         // runs `docker model package` for Hugging Face models
	Out    io.Writer
	Log    *slog.Logger
}

// Present reports whether the runner already has the model.
func (p *Puller) Present(ctx context.Context, input string) (bool, error) {
	ref, err := modelref.Parse(input)
	if err != nil {
		return false, err
	}
	m, err := p.Runner.Inspect(ctx, ref.String())
	return m != nil, err
}

// Pull downloads the model and, with a Runner, loads it.
func (p *Puller) Pull(ctx context.Context, input string) error {
	ref, err := modelref.Parse(input)
	if err != nil {
		return err
	}
	if ref.HF {
		return p.pullHF(ctx, ref)
	}
	return p.pullOCI(ctx, ref)
}

func (p *Puller) pullOCI(ctx context.Context, ref modelref.Ref) error {
	fmt.Fprintf(p.Out, "Resolving %s\n", ref)
	img, err := p.Images.Registry.ResolveModel(ctx, ref.OCI)
	if err != nil {
		return err
	}
	if p.Runner != nil {
		if m, err := p.Runner.Inspect(ctx, ref.String()); err == nil && m != nil && m.ID == img.ManifestDigest {
			fmt.Fprintf(p.Out, "%s is up to date.\n", ref)
			return nil
		}
	}
	warnMemory(p.Out, layerBytes(img))
	if err := p.Images.Fetch(ctx, img, p.Opts); err != nil {
		return err
	}
	if p.Runner == nil {
		fmt.Fprintf(p.Out, "Downloaded %s into the ddm cache (not loaded into Docker Model Runner).\n", ref)
		return nil
	}

	fmt.Fprintf(p.Out, "Loading %s into Docker Model Runner\n", ref)
	a := modelrunner.Archive{RawManifest: img.RawManifest, Config: img.Config.Digest, Open: p.Images.Cache.Open}
	for _, l := range img.Layers {
		a.Layers = append(a.Layers, l.Digest)
	}
	if err := p.load(ctx, a); err != nil {
		return err
	}
	// The runner identifies models by manifest digest, which the archive
	// preserved; naming it makes it show up exactly as after `docker model pull`.
	if err := p.Runner.Tag(ctx, img.ManifestDigest, ref.Name, ref.Tag); err != nil {
		return err
	}
	fmt.Fprintf(p.Out, "Loaded model %s\n", ref)
	return nil
}

func layerBytes(img *registry.Image) int64 {
	var n int64
	for _, l := range img.Layers {
		n += l.Size
	}
	return n
}

// load streams the archive into the runner. A blob that fails its digest
// check aborts the stream before the manifest is sent, so nothing is
// registered, and is removed from the cache to be downloaded again.
func (p *Puller) load(ctx context.Context, a modelrunner.Archive) error {
	pr, pw := io.Pipe()
	werr := make(chan error, 1)
	go func() {
		err := modelrunner.WriteModelTar(pw, a)
		pw.CloseWithError(err)
		werr <- err
	}()
	err := p.Runner.Load(ctx, pr)
	_ = pr.Close() // unblock the writer if the runner stopped reading
	writeErr := <-werr
	if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
		var ce *docker.CorruptBlobError
		if errors.As(writeErr, &ce) {
			_ = os.Remove(p.Images.Cache.BlobPath(ce.Digest))
			return fmt.Errorf("%w; it was removed from the cache, run the command again to download it again", writeErr)
		}
		return writeErr
	}
	if errors.Is(err, modelrunner.ErrNoLoadAPI) {
		return fmt.Errorf("%w: update Docker Desktop or the runner (`docker model reinstall-runner`), or use plain `docker model pull`", err)
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (p *Puller) pullHF(ctx context.Context, ref modelref.Ref) error {
	fmt.Fprintf(p.Out, "Looking up %s on the Hugging Face Hub\n", ref.Repo)
	files, endpoint, err := p.Hub.ListFiles(ctx, ref.Repo, hfRevision)
	if err != nil {
		return err
	}
	if endpoint != p.Hub.Endpoints[len(p.Hub.Endpoints)-1] {
		fmt.Fprintf(p.Out, "Listed through %s\n", endpoint)
	}
	plan, err := hf.Select(files, ref.Tag)
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if p.Runner != nil {
		if m, err := p.Runner.Inspect(ctx, ref.String()); err == nil && m != nil {
			fmt.Fprintf(p.Out, "%s is already in Docker Model Runner.\n", ref)
			return nil
		}
	}
	warnMemory(p.Out, plan.WeightBytes())
	digests, err := p.downloadHF(ctx, ref, plan)
	if err != nil {
		return err
	}
	p.rememberHF(ref, plan, digests)
	if p.Runner == nil {
		fmt.Fprintf(p.Out, "Downloaded %s into the ddm cache (not loaded into Docker Model Runner).\n", ref)
		return nil
	}
	return p.packageHF(ctx, ref, plan, digests)
}

// downloadHF fetches every file of the plan into the cache and returns each
// file's digest by path. Weights (Git LFS files) go through the resumable
// engine and are verified against the Hub's sha256; small files are fetched
// in one request.
func (p *Puller) downloadHF(ctx context.Context, ref modelref.Ref, plan *hf.Plan) (map[string]string, error) {
	digests := map[string]string{}
	var blobs []engine.Blob
	sources := map[string][]engine.Source{}
	var total int64
	for _, f := range plan.Files() {
		if f.SHA256 == "" {
			b, err := p.Hub.Fetch(ctx, ref.Repo, hfRevision, f.Path)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(b)
			d := "sha256:" + hex.EncodeToString(sum[:])
			if err := p.Images.Cache.PutBytes(d, b); err != nil {
				return nil, err
			}
			digests[f.Path] = d
			continue
		}
		d := "sha256:" + strings.ToLower(f.SHA256)
		digests[f.Path] = d
		if _, queued := sources[d]; queued || p.Images.Cache.Has(d, f.Size) {
			continue
		}
		blobs = append(blobs, engine.Blob{Digest: d, Size: f.Size, Name: path.Base(f.Path)})
		sources[d] = p.Hub.Sources(ref.Repo, hfRevision, f.Path)
		total += f.Size
	}
	if len(blobs) == 0 {
		return digests, nil
	}
	if err := p.Images.CheckSpace(total); err != nil {
		return nil, err
	}
	fmt.Fprintf(p.Out, "Downloading %d file(s) (%s)\n", len(blobs), ui.HumanBytes(total))

	var rep pull.Reporter
	var prog engine.Progress
	if p.Images.NewReporter != nil {
		rep = p.Images.NewReporter(total)
		prog = rep
	}
	dl := &engine.Downloader{Cache: p.Images.Cache, Options: p.Opts.Engine, Progress: prog, Log: p.Log}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(p.Opts.ParallelBlobs, 1))
	for _, b := range blobs {
		g.Go(func() error { return dl.Download(gctx, b, sources[b.Digest]) })
	}
	err := g.Wait()
	if rep != nil {
		rep.Close()
	}
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return digests, err
}

// rememberHF records the model's files in the cache index for `ddm cache`.
func (p *Puller) rememberHF(ref modelref.Ref, plan *hf.Plan, digests map[string]string) {
	r := cache.Ref{Name: ref.String(), Kind: "model"}
	for _, f := range plan.Files() {
		r.Blobs = append(r.Blobs, digests[f.Path])
		r.Size += f.Size
	}
	if err := p.Images.Cache.Remember(r); err != nil && p.Log != nil {
		p.Log.Warn("could not update the cache index", "err", err)
	}
}

// packageHF builds the model with `docker model package` from the cached
// files, under their original names in a staging directory, so Docker's own
// tooling writes the config (format, quantization, parameters) and streams
// the result into the runner.
func (p *Puller) packageHF(ctx context.Context, ref modelref.Ref, plan *hf.Plan, digests map[string]string) error {
	root, err := filepath.Abs(filepath.Join(p.Images.Cache.Root, "partial"))
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "hf-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	local := func(f hf.File) string { return filepath.Join(dir, filepath.FromSlash(f.Path)) }
	for _, f := range plan.Files() {
		dst := local(f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := linkOrCopy(p.Images.Cache.BlobPath(digests[f.Path]), dst); err != nil {
			return fmt.Errorf("staging %s: %w", f.Path, err)
		}
	}

	args := []string{"model", "package"}
	switch plan.Format {
	case hf.GGUF:
		args = append(args, "--gguf", local(plan.Weights[0]))
		if plan.MMProj != nil {
			args = append(args, "--mmproj", local(*plan.MMProj))
		}
		if plan.ChatTemplate != nil {
			args = append(args, "--chat-template", local(*plan.ChatTemplate))
		}
	case hf.Safetensors:
		args = append(args, "--safetensors-dir", dir)
	}

	fmt.Fprintf(p.Out, "Packaging %s for Docker Model Runner\n", ref)
	// The CLI only accepts lowercase repository names, but Hugging Face
	// names keep their case. Package under the lowercase name, then add the
	// exact name through the runner's API.
	lower := strings.ToLower(ref.Name) + ":" + ref.Tag
	if lower != ref.String() {
		if tail, err := p.Docker.Stream(ctx, p.Out, append(args, lower)...); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return packageError(tail)
		}
		if err := p.Runner.Tag(ctx, lower, ref.Name, ref.Tag); err != nil {
			return fmt.Errorf("the model was loaded as %s, but could not also be named %s: %w", lower, ref, err)
		}
		fmt.Fprintf(p.Out, "Loaded model %s\n", ref)
		return nil
	}
	tail, err := p.Docker.Stream(ctx, p.Out, append(args, ref.String())...)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return packageError(tail)
	}
	fmt.Fprintf(p.Out, "Loaded model %s\n", ref)
	return nil
}

// packageError explains a failed `docker model package`.
func packageError(tail string) error {
	err := fmt.Errorf("docker model package failed: %s", lastLine(tail))
	if strings.Contains(tail, "missing blob") {
		return fmt.Errorf("%w\nDocker Model Runner refused the files ddm downloaded (they are verified and kept, so a retry won't download them again). "+
			"Restart Docker Desktop and run the command again; if `docker model df` also fails, the runner's own store is damaged", err)
	}
	return err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// warnMemory warns (without blocking) when the weights probably won't fit.
func warnMemory(w io.Writer, weights int64) {
	if weights <= 0 {
		return
	}
	if total, err := sysmem.Total(); err == nil && total > 0 && float64(weights) > 0.7*float64(total) {
		fmt.Fprintf(w, "Warning: this model's weights (%s) need most or more of this machine's memory (%s); it may load slowly or not at all.\n",
			ui.HumanBytes(weights), ui.HumanBytes(int64(total)))
	}
	if vram := sysmem.GPUMemory(); vram > 0 && uint64(weights) > vram {
		fmt.Fprintf(w, "Note: the weights don't fit in GPU memory (%s), so part of the model will run on the CPU.\n", ui.HumanBytes(int64(vram)))
	}
}
