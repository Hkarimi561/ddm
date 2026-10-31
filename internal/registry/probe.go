package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Probe is the result of measuring one endpoint.
type Probe struct {
	Endpoint Endpoint
	Latency  time.Duration // time to authenticate and fetch the manifest
	Bytes    int64         // bytes of the sample layer downloaded
	Elapsed  time.Duration // time spent downloading them
	Err      error
}

// Speed returns the measured throughput in bytes per second.
func (p Probe) Speed() float64 {
	if p.Elapsed <= 0 {
		return 0
	}
	return float64(p.Bytes) / p.Elapsed.Seconds()
}

// Ping checks that ep answers the registry API at /v2/ and returns the round
// trip time. "401 Unauthorized" counts as an answer: it is how registries
// ask for a token.
func (c *Client) Ping(ctx context.Context, ep Endpoint) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL()+"/v2/", nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := (&http.Client{Transport: c.Transport}).Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	rtt := time.Since(start)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		return rtt, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return rtt, nil
}

// ProbeEndpoint measures how well ep serves ref: the latency of fetching its
// manifest, then the throughput of downloading the first maxBytes of its
// largest layer, giving up on the download after maxTime.
func (c *Client) ProbeEndpoint(ctx context.Context, ep Endpoint, ref name.Reference, platform v1.Platform, maxBytes int64, maxTime time.Duration) Probe {
	pr := Probe{Endpoint: ep}
	start := time.Now()
	img, err := c.resolveAt(ctx, ep, ref, platform)
	pr.Latency = time.Since(start)
	if err != nil {
		pr.Err = err
		return pr
	}
	layer, ok := img.LargestLayer()
	if !ok {
		pr.Err = errors.New("the test image has no layers")
		return pr
	}
	be, err := c.BlobEndpoint(ctx, ep, ref.Context())
	if err != nil {
		pr.Err = err
		return pr
	}

	dctx, cancel := context.WithTimeout(ctx, maxTime)
	defer cancel()
	src := be.Source(layer.Digest)
	n := min(maxBytes, layer.Size)
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, src.URL, nil)
	if err != nil {
		pr.Err = err
		return pr
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	t0 := time.Now()
	resp, err := src.Client.Do(req)
	if err != nil {
		pr.Err = err
		return pr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		pr.Err = fmt.Errorf("HTTP %d downloading a layer", resp.StatusCode)
		return pr
	}
	got, err := io.Copy(io.Discard, io.LimitReader(resp.Body, n))
	pr.Bytes, pr.Elapsed = got, time.Since(t0)
	// Running out of time is expected on slow links: what arrived so far
	// is the measurement.
	if err != nil && dctx.Err() == nil {
		pr.Err = err
	} else if got == 0 {
		pr.Err = fmt.Errorf("no data within %s", maxTime)
	}
	return pr
}
