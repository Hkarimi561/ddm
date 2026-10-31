// Package modelrunner talks to Docker Model Runner's API to check for and
// load models. Models are pushed into the runner (see
// docs/model-runner-research.md), so it never has to reach ddm, wherever
// it runs.
package modelrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrNoLoadAPI means the runner doesn't offer the load endpoint ddm uses,
// e.g. because it is too old.
var ErrNoLoadAPI = errors.New("this Docker Model Runner has no model load API")

// Kind says how the runner was reached.
type Kind string

const (
	KindDesktop Kind = "Docker Desktop"
	KindEngine  Kind = "Docker Engine"
	KindManual  Kind = "MODEL_RUNNER_HOST"
)

// Client is a Model Runner API endpoint.
type Client struct {
	Kind Kind
	Base string // e.g. "http://localhost:12434", or "http://localhost/exp/vDD4.40" via the engine socket
	HTTP *http.Client
}

// Model is the part of the runner's model description ddm uses.
type Model struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.HTTP.Do(req)
}

func errorFrom(resp *http.Response, what string) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s: %s", what, msg)
}

// List returns the runner's models.
func (c *Client) List(ctx context.Context) ([]Model, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errorFrom(resp, "listing models")
	}
	var models []Model
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("listing models: %w", err)
	}
	return models, nil
}

// Inspect returns the model the runner knows by name ("ai/smollm2:latest"),
// or nil if it has none.
func (c *Client) Inspect(ctx context.Context, name string) (*Model, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models/"+name, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var m Model
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, fmt.Errorf("inspecting %s: %w", name, err)
		}
		return &m, nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, errorFrom(resp, "inspecting "+name)
}

// Load streams a model archive (see WriteModelTar) into the runner. The
// runner writes the manifest only after every blob, so an interrupted load
// leaves no model behind.
func (c *Client) Load(ctx context.Context, archive io.Reader) error {
	resp, err := c.do(ctx, http.MethodPost, "/models/load", archive, "application/x-tar")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		_, _ = io.Copy(io.Discard, resp.Body) // progress messages
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return ErrNoLoadAPI
	}
	return errorFrom(resp, "loading model")
}

// Tag gives the model source (a name or ID) the name repo:tag.
func (c *Client) Tag(ctx context.Context, source, repo, tag string) error {
	q := url.Values{"repo": {repo}, "tag": {tag}}
	resp, err := c.do(ctx, http.MethodPost, "/models/"+source+"/tag?"+q.Encode(), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return errorFrom(resp, fmt.Sprintf("tagging %s as %s:%s", source, repo, tag))
	}
	return nil
}
