package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/uiauth"
)

type uiClient struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *uiClient) do(method, path string, body any, header bool) (int, map[string]any) {
	c.t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	if header {
		req.Header.Set("X-DDM", "1")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newTestUI(t *testing.T) (*uiClient, *globals, string) {
	t.Helper()
	dir := t.TempDir()
	g := &globals{configPath: filepath.Join(dir, "config", "config.yaml"), cacheDir: filepath.Join(dir, "cache")}
	pwPath := uiauth.PasswordPath(g.configPath)
	if err := uiauth.Set(pwPath, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newUIServer(g, pwPath))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &uiClient{t: t, base: srv.URL, http: &http.Client{Jar: jar}}, g, pwPath
}

func TestUIRequiresLogin(t *testing.T) {
	c, _, _ := newTestUI(t)
	if code, _ := c.do("GET", "/", nil, false); code != http.StatusOK {
		t.Fatalf("the page itself must load without login, got %d", code)
	}
	for _, p := range []string{"/api/config", "/api/status", "/api/cache", "/api/me"} {
		if code, _ := c.do("GET", p, nil, true); code != http.StatusUnauthorized {
			t.Errorf("GET %s without login = %d, want 401", p, code)
		}
	}
	if code, body := c.do("POST", "/api/login", map[string]string{"password": "wrong password"}, true); code != http.StatusUnauthorized || body["error"] == nil {
		t.Errorf("wrong password = %d %v", code, body)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "correct horse battery"}, false); code != http.StatusForbidden {
		t.Errorf("a POST without the X-DDM header must be refused (CSRF), got %d", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "correct horse battery"}, true); code != http.StatusOK {
		t.Fatalf("login failed: %d", code)
	}
	if code, _ := c.do("GET", "/api/me", nil, true); code != http.StatusOK {
		t.Errorf("after login /api/me = %d", code)
	}
	c.do("POST", "/api/logout", map[string]any{}, true)
	if code, _ := c.do("GET", "/api/me", nil, true); code != http.StatusUnauthorized {
		t.Errorf("after logout /api/me = %d, want 401", code)
	}
}

func TestUISettingsAndMirrors(t *testing.T) {
	c, g, _ := newTestUI(t)
	c.do("POST", "/api/login", map[string]string{"password": "correct horse battery"}, true)

	code, body := c.do("POST", "/api/config", map[string]string{"key": "bandwidth_limit", "value": "2MiB"}, true)
	if code != http.StatusOK || body["value"] != "2MiB" {
		t.Fatalf("set bandwidth_limit = %d %v", code, body)
	}
	if code, body := c.do("POST", "/api/config", map[string]string{"key": "connections", "value": "zero"}, true); code != http.StatusBadRequest || body["error"] == nil {
		t.Errorf("an invalid value was accepted: %d %v", code, body)
	}
	if code, _ := c.do("PUT", "/api/mirrors", map[string]any{"registry": "docker.io", "urls": []string{"https://mirror.example.com"}}, true); code != http.StatusOK {
		t.Fatalf("set mirrors = %d", code)
	}

	f, err := config.Load(g.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if f.Config.RateLimit != 2<<20 {
		t.Errorf("bandwidth_limit not saved: %d", f.Config.RateLimit)
	}
	if _, urls := f.Mirrors("docker.io"); len(urls) != 1 {
		t.Errorf("mirrors not saved: %v", urls)
	}

	_, cfg := c.do("GET", "/api/config", nil, true)
	settings, _ := cfg["settings"].([]any)
	if len(settings) != len(config.Keys) {
		t.Errorf("got %d settings, want %d", len(settings), len(config.Keys))
	}

	if code, body := c.do("POST", "/api/config/unset", map[string]string{"key": "bandwidth_limit"}, true); code != http.StatusOK || body["value"] != "0" {
		t.Errorf("unset = %d %v", code, body)
	}
	if code, body := c.do("POST", "/api/pause", map[string]int{"pid": 0}, true); code != http.StatusOK {
		t.Errorf("pause with nothing running = %d %v", code, body)
	}
	if code, _ := c.do("GET", "/api/cache", nil, true); code != http.StatusOK {
		t.Errorf("cache = %d", code)
	}
}

func TestUIPasswordChangeLogsOthersOut(t *testing.T) {
	a, _, pwPath := newTestUI(t)
	// A second browser on the same server.
	jar, _ := cookiejar.New(nil)
	b := &uiClient{t: t, base: a.base, http: &http.Client{Jar: jar}}
	a.do("POST", "/api/login", map[string]string{"password": "correct horse battery"}, true)
	b.do("POST", "/api/login", map[string]string{"password": "correct horse battery"}, true)

	if code, _ := a.do("POST", "/api/password", map[string]string{"current": "nope", "new": "another long one"}, true); code != http.StatusBadRequest {
		t.Errorf("wrong current password accepted: %d", code)
	}
	if code, _ := a.do("POST", "/api/password", map[string]string{"current": "correct horse battery", "new": "another long one"}, true); code != http.StatusOK {
		t.Fatalf("password change failed: %d", code)
	}
	if code, _ := a.do("GET", "/api/me", nil, true); code != http.StatusOK {
		t.Error("the browser that changed the password was logged out")
	}
	if code, _ := b.do("GET", "/api/me", nil, true); code != http.StatusUnauthorized {
		t.Error("the other browser is still logged in")
	}
	if ok, _ := uiauth.Check(pwPath, "another long one"); !ok {
		t.Error("the new password was not stored")
	}
	b2, _ := os.ReadFile(pwPath)
	if strings.Contains(string(b2), "another long one") {
		t.Error("the password is stored in plain text")
	}
}
