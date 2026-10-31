// Package registry talks to OCI distribution (v2) registries and their
// mirrors: it resolves image references to platform-specific manifests and
// hands out authenticated blob sources for the download engine.
package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/net/http/httpproxy"

	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/retry"
)

const userAgent = "ddm"

// Client resolves images and blob sources.
type Client struct {
	// Keychain supplies credentials. authn.DefaultKeychain reads
	// ~/.docker/config.json, including credential helpers.
	Keychain   authn.Keychain
	Transport  http.RoundTripper
	MaxRetries int // 0 = retry transient failures forever
	// Mirrors maps upstream registries ("docker.io") to mirror URLs, tried
	// in order before the upstream itself (see Endpoints).
	Mirrors map[string][]string
	Log     *slog.Logger
}

// NewClient returns a client using Docker's credentials and the given
// transport.
func NewClient(t http.RoundTripper, mirrors map[string][]string, log *slog.Logger) *Client {
	return &Client{Keychain: authn.DefaultKeychain, Transport: t, Mirrors: mirrors, Log: log}
}

// NewTransport returns the base HTTP transport. It speaks HTTP/1.1 only:
// segmented downloads help on lossy links because every segment gets its own
// TCP connection, and HTTP/2 would multiplex them all over a single one.
func NewTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	return &http.Transport{
		Proxy: proxy,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   64,
		// A non-nil, empty map disables HTTP/2.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// ProxyFunc returns the proxy selection for all registry traffic: the
// configured proxy (http://, https:// or socks5://) when set, otherwise
// HTTP_PROXY / HTTPS_PROXY. Hosts in noProxy (default $NO_PROXY) bypass it.
func ProxyFunc(proxy, noProxy string) func(*http.Request) (*url.URL, error) {
	if proxy == "" {
		return http.ProxyFromEnvironment
	}
	if noProxy == "" {
		noProxy = os.Getenv("NO_PROXY")
		if noProxy == "" {
			noProxy = os.Getenv("no_proxy")
		}
	}
	pf := (&httpproxy.Config{HTTPProxy: proxy, HTTPSProxy: proxy, NoProxy: noProxy}).ProxyFunc()
	return func(r *http.Request) (*url.URL, error) { return pf(r.URL) }
}

// Descriptor describes a blob.
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
}

// Image is a resolved, platform-specific image manifest.
type Image struct {
	Ref               name.Reference // the reference as requested, on the upstream registry
	Source            string         // endpoint the manifest came from
	Platform          v1.Platform
	IndexDigest       string // set when Ref names a multi-platform index
	ManifestDigest    string
	ManifestMediaType string
	RawManifest       []byte
	Config            Descriptor
	Layers            []Descriptor
}

// LargestLayer returns the biggest layer, or false if there is none.
func (img *Image) LargestLayer() (Descriptor, bool) {
	var best Descriptor
	for _, l := range img.Layers {
		if l.Size > best.Size {
			best = l
		}
	}
	return best, best.Digest != ""
}

// ResolveImage fetches the manifest for ref from the first endpoint that can
// serve it, picking the entry for platform when ref names a multi-platform
// index. Transient failures are retried, cycling through the endpoints.
func (c *Client) ResolveImage(ctx context.Context, ref name.Reference, platform v1.Platform) (*Image, error) {
	eps := c.Endpoints(ref.Context().Registry)
	var img *Image
	var upstreamErr error
	err := c.retry(ctx, "fetching manifest", func() error {
		var firstTransient error
		for _, ep := range eps {
			im, err := c.resolveAt(ctx, ep, ref, platform)
			if err == nil {
				img = im
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if len(eps) > 1 {
				c.logger().Warn("could not fetch manifest; trying the next mirror", "from", ep.Name, "err", err)
			}
			if ep.Upstream {
				upstreamErr = err
			}
			if firstTransient == nil && transient(err) {
				firstTransient = fmt.Errorf("%s: %w", ep.Name, err)
			}
		}
		if firstTransient != nil {
			return firstTransient
		}
		// Every endpoint gave a definite answer; the upstream's is the one
		// that counts (a mirror may simply not have the image).
		return upstreamErr
	})
	if err != nil {
		return nil, describe(ref, platform, err)
	}
	return img, nil
}

// ResolveAt fetches the manifest for ref from one endpoint, without retries.
func (c *Client) ResolveAt(ctx context.Context, ep Endpoint, ref name.Reference, platform v1.Platform) (*Image, error) {
	return c.resolveAt(ctx, ep, ref, platform)
}

func (c *Client) resolveAt(ctx context.Context, ep Endpoint, ref name.Reference, platform v1.Platform) (*Image, error) {
	desc, err := remote.Get(ep.Ref(ref), c.options(ctx, platform)...)
	if err != nil {
		return nil, err
	}
	img, err := desc.Image()
	if err != nil {
		return nil, err
	}
	raw, err := img.RawManifest()
	if err != nil {
		return nil, err
	}
	mt, err := img.MediaType()
	if err != nil {
		return nil, err
	}
	dgst, err := img.Digest()
	if err != nil {
		return nil, err
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, err
	}

	out := &Image{
		Ref:               ref,
		Source:            ep.Name,
		Platform:          platform,
		ManifestDigest:    dgst.String(),
		ManifestMediaType: string(mt),
		RawManifest:       raw,
		Config: Descriptor{
			MediaType: string(m.Config.MediaType),
			Digest:    m.Config.Digest.String(),
			Size:      m.Config.Size,
		},
	}
	if desc.MediaType.IsIndex() {
		out.IndexDigest = desc.Digest.String()
	}
	if !isImageConfig(m.Config.MediaType) {
		return nil, &notImageError{ref: ref, mediaType: string(m.Config.MediaType)}
	}
	for _, l := range m.Layers {
		if !l.MediaType.IsDistributable() {
			return nil, fmt.Errorf("%s uses non-distributable (foreign) layers, which ddm does not support yet", ref)
		}
		out.Layers = append(out.Layers, Descriptor{MediaType: string(l.MediaType), Digest: l.Digest.String(), Size: l.Size})
	}
	return out, nil
}

type notImageError struct {
	ref       name.Reference
	mediaType string
}

func (e *notImageError) Error() string {
	return fmt.Sprintf("%s is not a container image (its config type is %s); if it is a Docker model, use `ddm model pull`", e.ref, e.mediaType)
}

func isImageConfig(mt types.MediaType) bool {
	return mt == types.DockerConfigJSON || mt == types.OCIConfigJSON
}

// BlobEndpoint is an authenticated handle on one endpoint's copy of a
// repository's blobs.
type BlobEndpoint struct {
	Name   string
	base   string
	client *http.Client
}

// Source returns the engine source for one blob.
func (e *BlobEndpoint) Source(digest string) engine.Source {
	return engine.Source{Name: e.Name, URL: e.base + digest, Client: e.client}
}

// Sources returns one engine source per endpoint, in order.
func Sources(eps []*BlobEndpoint, digest string) []engine.Source {
	out := make([]engine.Source, len(eps))
	for i, e := range eps {
		out[i] = e.Source(digest)
	}
	return out
}

// BlobEndpoints authenticates against every endpoint that serves repo and
// returns those that answered, in order. Endpoints that are down are
// skipped; it fails only when none can be reached.
func (c *Client) BlobEndpoints(ctx context.Context, repo name.Repository) ([]*BlobEndpoint, error) {
	eps := c.Endpoints(repo.Registry)
	var out []*BlobEndpoint
	err := c.retry(ctx, "connecting to the registry", func() error {
		var firstErr error
		for _, ep := range eps {
			be, err := c.BlobEndpoint(ctx, ep, repo)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.logger().Warn("skipping unreachable mirror", "mirror", ep.Name, "err", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", ep.Name, err)
				}
				continue
			}
			out = append(out, be)
		}
		if len(out) == 0 {
			return firstErr
		}
		return nil
	})
	return out, err
}

// BlobEndpoint authenticates against one endpoint (anonymous, basic or
// bearer token, as it demands) and returns a blob endpoint whose client
// refreshes tokens as they expire.
func (c *Client) BlobEndpoint(ctx context.Context, ep Endpoint, upstream name.Repository) (*BlobEndpoint, error) {
	repo := ep.Repo(upstream)
	auth, err := c.Keychain.Resolve(repo)
	if err != nil {
		return nil, fmt.Errorf("reading credentials for %s: %w", repo.RegistryStr(), err)
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, auth, c.Transport, []string{repo.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	return &BlobEndpoint{
		Name:   ep.Name,
		base:   fmt.Sprintf("%s://%s/v2/%s/blobs/", repo.Scheme(), repo.RegistryStr(), repo.RepositoryStr()),
		client: &http.Client{Transport: rt},
	}, nil
}

func (c *Client) options(ctx context.Context, platform v1.Platform) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.Keychain),
		remote.WithTransport(c.Transport),
		remote.WithPlatform(platform),
		remote.WithUserAgent(userAgent),
	}
}

func (c *Client) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// retry runs fn until it succeeds, fails permanently, or ctx is done.
func (c *Client) retry(ctx context.Context, what string, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !transient(err) || (c.MaxRetries > 0 && attempt >= c.MaxRetries) {
			return err
		}
		delay := retry.Backoff(attempt, time.Second, 30*time.Second)
		c.logger().Warn(what+" failed; retrying", "attempt", attempt, "in", delay.Round(100*time.Millisecond), "err", err)
		if err := retry.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// transient reports whether err looks like a network or server hiccup that
// is worth retrying, as opposed to a bad name, missing image or bad
// credentials.
func transient(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode == http.StatusRequestTimeout || te.StatusCode == http.StatusTooManyRequests || te.StatusCode >= 500
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func describe(ref name.Reference, platform v1.Platform, err error) error {
	var te *transport.Error
	if errors.As(err, &te) {
		switch te.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("access to %s was denied: the image may not exist, or you may need to run `docker login`\n  (%w)", ref, err)
		case http.StatusNotFound:
			return fmt.Errorf("%s was not found: check the image name and tag\n  (%w)", ref, err)
		}
	}
	var nie *notImageError
	if errors.As(err, &nie) {
		return err
	}
	if !transient(err) {
		return fmt.Errorf("resolving %s for %s: %w (use --platform to pick another platform)", ref, platform.String(), err)
	}
	return fmt.Errorf("resolving %s: %w", ref, err)
}
