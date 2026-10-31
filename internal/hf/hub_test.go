package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHub serves a tree listing (in two pages) and file downloads, and
// records which requests carried the token.
type fakeHub struct {
	mu       sync.Mutex
	authSeen map[string]bool
	gated    bool
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.authSeen[r.URL.Path] = r.Header.Get("Authorization") == "Bearer secret"
	h.mu.Unlock()
	if h.gated && r.Header.Get("Authorization") == "" {
		http.Error(w, "gated", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/models/org/Model-GGUF/tree/main" && r.URL.Query().Get("cursor") == "":
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/models/org/Model-GGUF/tree/main?recursive=true&cursor=2>; rel="next"`, r.Host))
		json.NewEncoder(w).Encode([]map[string]any{
			{"type": "directory", "path": "sub"},
			{"type": "file", "path": "README.md", "size": 10, "oid": "abc"},
			{"type": "file", "path": "m-Q4_K_M.gguf", "size": 4, "lfs": map[string]any{"oid": strings.Repeat("1", 64), "size": 1000}},
		})
	case r.URL.Path == "/api/models/org/Model-GGUF/tree/main":
		json.NewEncoder(w).Encode([]map[string]any{
			{"type": "file", "path": "sub/m-Q8_0.gguf", "size": 4, "lfs": map[string]any{"oid": strings.Repeat("2", 64), "size": 2000}},
		})
	case r.URL.Path == "/org/Model-GGUF/resolve/main/README.md":
		io.WriteString(w, "hello")
	default:
		http.NotFound(w, r)
	}
}

func TestListFilesPagesAndUsesLFSInfo(t *testing.T) {
	hub := &fakeHub{authSeen: map[string]bool{}}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	h := &Hub{Endpoints: []string{srv.URL}, Token: "secret"}

	files, base, err := h.ListFiles(context.Background(), "org/Model-GGUF", "main")
	if err != nil {
		t.Fatal(err)
	}
	if base != srv.URL || len(files) != 3 {
		t.Fatalf("got %d files from %s: %+v", len(files), base, files)
	}
	q8 := files[2]
	if q8.Path != "sub/m-Q8_0.gguf" || q8.Size != 2000 || q8.SHA256 != strings.Repeat("2", 64) {
		t.Errorf("LFS file = %+v", q8)
	}
	if files[0].SHA256 != "" {
		t.Errorf("non-LFS file got a sha256: %+v", files[0])
	}
	if !hub.authSeen["/api/models/org/Model-GGUF/tree/main"] {
		t.Error("the token was not sent to the Hub")
	}

	b, err := h.Fetch(context.Background(), "org/Model-GGUF", "main", "README.md")
	if err != nil || string(b) != "hello" {
		t.Fatalf("Fetch = %q, %v", b, err)
	}
}

func TestListFilesFallsBackAndExplainsGating(t *testing.T) {
	gated := &fakeHub{authSeen: map[string]bool{}, gated: true}
	srv := httptest.NewServer(gated)
	defer srv.Close()

	h := &Hub{Endpoints: []string{"http://127.0.0.1:1", srv.URL}} // the first endpoint is down
	_, _, err := h.ListFiles(context.Background(), "org/Model-GGUF", "main")
	if err == nil || !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Fatalf("got %v, want an explanation mentioning HF_TOKEN", err)
	}
	h.Token = "secret"
	if _, base, err := h.ListFiles(context.Background(), "org/Model-GGUF", "main"); err != nil || base != srv.URL {
		t.Fatalf("with a token: %s %v", base, err)
	}
}

func TestTokenNotSentToOtherHosts(t *testing.T) {
	var cdnAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		io.WriteString(w, "data")
	}))
	defer cdn.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/blob", http.StatusFound)
	}))
	defer hub.Close()

	h := &Hub{Endpoints: []string{hub.URL}, Token: "secret"}
	if _, err := h.Fetch(context.Background(), "org/m", "main", "x"); err != nil {
		t.Fatal(err)
	}
	if cdnAuth != "" {
		t.Errorf("the token leaked to the CDN host: %q", cdnAuth)
	}
}

func TestNewHubEndpoints(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	h := NewHub(map[string][]string{"hf.co": {"https://hf-mirror.com/"}, "docker.io": {"https://x"}}, nil)
	if want := []string{"https://hf-mirror.com", DefaultEndpoint}; strings.Join(h.Endpoints, " ") != strings.Join(want, " ") {
		t.Errorf("endpoints %v, want %v", h.Endpoints, want)
	}
	h = NewHub(map[string][]string{"huggingface.co": {"https://huggingface.co", "hf-mirror.com"}}, nil)
	if want := []string{DefaultEndpoint, "https://hf-mirror.com"}; strings.Join(h.Endpoints, " ") != strings.Join(want, " ") {
		t.Errorf("explicit upstream position: %v, want %v", h.Endpoints, want)
	}
	t.Setenv("HF_ENDPOINT", "https://hub.internal/")
	if h = NewHub(nil, nil); len(h.Endpoints) != 1 || h.Endpoints[0] != "https://hub.internal" {
		t.Errorf("HF_ENDPOINT ignored: %v", h.Endpoints)
	}
}

func TestFileURL(t *testing.T) {
	got := FileURL("https://huggingface.co", "org/Model", "main", "sub dir/m Q4.gguf")
	if want := "https://huggingface.co/org/Model/resolve/main/sub%20dir/m%20Q4.gguf"; got != want {
		t.Errorf("FileURL = %s, want %s", got, want)
	}
}
