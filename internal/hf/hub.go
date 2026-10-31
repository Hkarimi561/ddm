// Package hf lists and downloads model files from the Hugging Face Hub (or
// a mirror of it), choosing the same files Docker Model Runner would.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/engine"
)

// DefaultEndpoint is the Hugging Face Hub.
const DefaultEndpoint = "https://huggingface.co"

// File is a file in a repository.
type File struct {
	Path   string
	Size   int64
	SHA256 string // hex digest, for files stored with Git LFS (all model weights); empty otherwise
}

// Hub talks to the Hub and its mirrors.
type Hub struct {
	Endpoints []string // base URLs, tried in order
	Token     string   // HF_TOKEN, sent only to the endpoints
	Transport http.RoundTripper
}

// NewHub builds a Hub from the configured mirrors ("hf.co" or
// "huggingface.co" entries), $HF_ENDPOINT and $HF_TOKEN. The Hub itself
// comes last unless the mirror list places it.
func NewHub(mirrors map[string][]string, t http.RoundTripper) *Hub {
	upstream := strings.TrimRight(os.Getenv("HF_ENDPOINT"), "/")
	if upstream == "" {
		upstream = DefaultEndpoint
	}
	var eps []string
	listed := false
	for key, urls := range mirrors {
		if config.NormalizeRegistry(key) != config.HuggingFace {
			continue
		}
		for _, u := range urls {
			base := strings.TrimRight(u, "/")
			if !strings.Contains(base, "://") {
				base = "https://" + base
			}
			if config.NormalizeRegistry(base) == config.HuggingFace {
				base, listed = upstream, true
			}
			eps = append(eps, base)
		}
	}
	if !listed {
		eps = append(eps, upstream)
	}
	return &Hub{Endpoints: eps, Token: os.Getenv("HF_TOKEN"), Transport: t}
}

// client returns an HTTP client for base that sends the token to base's
// host only, never to the CDNs downloads are redirected to.
func (h *Hub) client(base string) *http.Client {
	t := h.Transport
	if t == nil {
		t = http.DefaultTransport
	}
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = u.Host
	}
	return &http.Client{Transport: &authTransport{base: t, token: h.Token, host: host}}
}

type authTransport struct {
	base        http.RoundTripper
	token, host string
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.token != "" && r.URL.Host == t.host {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// ListFiles lists every file of repo at revision from the first endpoint
// that answers, and returns that endpoint.
func (h *Hub) ListFiles(ctx context.Context, repo, revision string) ([]File, string, error) {
	var errs []error
	for _, base := range h.Endpoints {
		files, err := h.listFrom(ctx, base, repo, revision)
		if err == nil {
			return files, base, nil
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %w", base, err))
	}
	return nil, "", errors.Join(errs...)
}

func (h *Hub) listFrom(ctx context.Context, base, repo, revision string) ([]File, error) {
	next := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", base, repo, url.PathEscape(revision))
	client := h.client(base)
	var files []File
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var entries []treeEntry
		switch resp.StatusCode {
		case http.StatusOK:
			err = json.NewDecoder(resp.Body).Decode(&entries)
		case http.StatusUnauthorized, http.StatusForbidden:
			err = fmt.Errorf("access to %s denied: the repository may be gated or private; accept its terms on huggingface.co and set HF_TOKEN", repo)
		case http.StatusNotFound:
			err = fmt.Errorf("repository %s (revision %s) not found", repo, revision)
		default:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			err = fmt.Errorf("listing %s: %s %s", repo, resp.Status, strings.TrimSpace(string(b)))
		}
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Type != "file" {
				continue
			}
			f := File{Path: e.Path, Size: e.Size}
			if e.LFS != nil {
				f.SHA256, f.Size = e.LFS.OID, e.LFS.Size
			}
			files = append(files, f)
		}
		next = ""
		if m := nextLink.FindStringSubmatch(link); m != nil {
			next = m[1]
		}
	}
	return files, nil
}

// FileURL is where a file is downloaded from on one endpoint.
func FileURL(base, repo, revision, path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s", base, repo, url.PathEscape(revision), strings.Join(segs, "/"))
}

// Sources returns one download source per endpoint for a file, for the
// engine to fail over between.
func (h *Hub) Sources(repo, revision, path string) []engine.Source {
	out := make([]engine.Source, len(h.Endpoints))
	for i, base := range h.Endpoints {
		out[i] = engine.Source{Name: base, URL: FileURL(base, repo, revision, path), Client: h.client(base)}
	}
	return out
}

// Fetch downloads a small file (configs, templates) in one request.
func (h *Hub) Fetch(ctx context.Context, repo, revision, path string) ([]byte, error) {
	var errs []error
	for _, base := range h.Endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, FileURL(base, repo, revision, path), nil)
		if err != nil {
			return nil, err
		}
		resp, err := h.client(base).Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err == nil {
					return b, nil
				}
				errs = append(errs, err)
				continue
			}
			resp.Body.Close()
			err = fmt.Errorf("%s", resp.Status)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %w", base, err))
	}
	return nil, fmt.Errorf("downloading %s: %w", path, errors.Join(errs...))
}
