// Package registry talks to OCI distribution (v2) registries: it resolves
// image references to platform-specific manifests and hands out
// authenticated blob endpoints for the download engine.
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
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/retry"
)

const userAgent = "ddm"

// Client resolves images and blob endpoints.
type Client struct {
	// Keychain supplies credentials. authn.DefaultKeychain reads
	// ~/.docker/config.json, including credential helpers.
	Keychain   authn.Keychain
	Transport  http.RoundTripper
	MaxRetries int // 0 = retry transient failures forever
	Log        *slog.Logger
}

// NewClient returns a client using Docker's credentials and NewTransport.
func NewClient(log *slog.Logger) *Client {
	return &Client{Keychain: authn.DefaultKeychain, Transport: NewTransport(), Log: log}
}

// NewTransport returns the base HTTP transport. It honors HTTP_PROXY,
// HTTPS_PROXY and NO_PROXY, and speaks HTTP/1.1 only: segmented downloads
// help on lossy links because every segment gets its own TCP connection,
// and HTTP/2 would multiplex them all over a single one.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
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

// Descriptor describes a blob.
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
}

// Image is a resolved, platform-specific image manifest.
type Image struct {
	Ref               name.Reference
	Platform          v1.Platform
	IndexDigest       string // set when Ref names a multi-platform index
	ManifestDigest    string
	ManifestMediaType string
	RawManifest       []byte
	Config            Descriptor
	Layers            []Descriptor
}

// ResolveImage fetches the manifest for ref, picking the entry for platform
// when ref names a multi-platform index. Transient failures are retried.
func (c *Client) ResolveImage(ctx context.Context, ref name.Reference, platform v1.Platform) (*Image, error) {
	var (
		desc *remote.Descriptor
		img  v1.Image
		raw  []byte
	)
	err := c.retry(ctx, "fetching manifest", func() error {
		var err error
		if desc, err = remote.Get(ref, c.options(ctx, platform)...); err != nil {
			return err
		}
		if img, err = desc.Image(); err != nil {
			return err
		}
		raw, err = img.RawManifest()
		return err
	})
	if err != nil {
		return nil, describe(ref, platform, err)
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
		return nil, fmt.Errorf("%s is not a container image (its config type is %s); if it is a Docker model, use `ddm model pull`",
			ref, m.Config.MediaType)
	}
	for _, l := range m.Layers {
		if !l.MediaType.IsDistributable() {
			return nil, fmt.Errorf("%s uses non-distributable (foreign) layers, which ddm does not support yet", ref)
		}
		out.Layers = append(out.Layers, Descriptor{MediaType: string(l.MediaType), Digest: l.Digest.String(), Size: l.Size})
	}
	return out, nil
}

func isImageConfig(mt types.MediaType) bool {
	return mt == types.DockerConfigJSON || mt == types.OCIConfigJSON
}

// BlobEndpoint is an authenticated handle on a repository's blobs.
type BlobEndpoint struct {
	name   string
	base   string
	client *http.Client
}

// Source returns the engine source for one blob.
func (e *BlobEndpoint) Source(digest string) engine.Source {
	return engine.Source{Name: e.name, URL: e.base + digest, Client: e.client}
}

// BlobEndpoint authenticates against repo's registry (anonymous, basic or
// bearer token, as the registry demands) and returns a blob endpoint whose
// client refreshes tokens as they expire.
func (c *Client) BlobEndpoint(ctx context.Context, repo name.Repository) (*BlobEndpoint, error) {
	var rt http.RoundTripper
	err := c.retry(ctx, "connecting to "+repo.RegistryStr(), func() error {
		auth, err := c.Keychain.Resolve(repo)
		if err != nil {
			return fmt.Errorf("reading credentials for %s: %w", repo.RegistryStr(), err)
		}
		rt, err = transport.NewWithContext(ctx, repo.Registry, auth, c.Transport, []string{repo.Scope(transport.PullScope)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &BlobEndpoint{
		name:   repo.RegistryStr(),
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
	if !transient(err) {
		return fmt.Errorf("resolving %s for %s: %w (use --platform to pick another platform)", ref, platform.String(), err)
	}
	return fmt.Errorf("resolving %s: %w", ref, err)
}
