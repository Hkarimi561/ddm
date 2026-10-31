package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Config.Connections != Defaults().Connections || len(f.Config.Mirrors) != 0 {
		t.Fatalf("got %+v", f.Config)
	}
}

func TestLoadParsesEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
# my settings
connections: 8
parallel: 2
retries: 5
stall_timeout: 1m
min_speed: 64KiB
proxy: socks5://127.0.0.1:1080
no_proxy: localhost,.corp
cache_dir: /data/ddm
mirrors:
  docker.io:
    - https://docker.arvancloud.ir
    - https://docker.io
  ghcr.io:
    - http://10.0.0.5:5000/ghcr-proxy
`)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := f.Config
	if c.Connections != 8 || c.Parallel != 2 || c.Retries != 5 || c.StallTimeout != time.Minute ||
		c.MinSpeed != 64<<10 || c.Proxy != "socks5://127.0.0.1:1080" || c.NoProxy != "localhost,.corp" || c.CacheDir != "/data/ddm" {
		t.Fatalf("got %+v", c)
	}
	key, urls := f.Mirrors("index.docker.io")
	if key != "docker.io" || len(urls) != 2 || urls[0] != "https://docker.arvancloud.ir" {
		t.Fatalf("docker.io mirrors = %q %v", key, urls)
	}
}

func TestLoadRejectsMistakes(t *testing.T) {
	cases := map[string]string{
		"unknown key":     "conections: 4\n",
		"bad number":      "connections: lots\n",
		"bad duration":    "stall_timeout: soon\n",
		"bad proxy":       "proxy: ftp://example.com\n",
		"bad mirror":      "mirrors:\n  docker.io:\n    - ftp://mirror\n",
		"mirrors not map": "mirrors: [a, b]\n",
		"not a mapping":   "- a\n- b\n",
	}
	for name, body := range cases {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, body)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: accepted %q", name, body)
		}
	}
}

func TestSetKeepsCommentsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "# keep me\nconnections: 4 # and me\n")
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set("connections", "6"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("min_speed", "1MiB"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("connections", "0"); err == nil {
		t.Fatal("connections=0 accepted")
	}
	if err := f.SetMirrors("docker.io", []string{"https://a.example", "https://b.example"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"# keep me", "# and me", "connections: 6", "min_speed: 1MiB", "https://b.example"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("saved file lacks %q:\n%s", want, data)
		}
	}

	g, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if g.Config.Connections != 6 || g.Config.MinSpeed != 1<<20 {
		t.Fatalf("reloaded %+v", g.Config)
	}
	if _, urls := g.Mirrors("registry-1.docker.io"); len(urls) != 2 {
		t.Fatalf("reloaded mirrors %v", urls)
	}

	if err := g.SetMirrors("index.docker.io", nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Unset("connections"); err != nil {
		t.Fatal(err)
	}
	if err := g.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "mirrors") || strings.Contains(string(data), "connections") {
		t.Errorf("removed entries still present:\n%s", data)
	}
}

func TestSaveCreatesNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "config.yaml")
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set("parallel", "5"); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	g, err := Load(path)
	if err != nil || g.Config.Parallel != 5 {
		t.Fatalf("reload: %v %+v", err, g)
	}
}

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"0": 0, "512": 512, "32KiB": 32 << 10, "32kib/s": 32 << 10, "2M": 2 << 20,
		"1.5MiB": 3 << 19, "1MB": 1000000, "1 GiB": 1 << 30, "10b": 10,
	}
	for in, want := range good {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "fast", "-1", "1XB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
	for _, n := range []int64{0, 1, 1000, 1 << 10, 64 << 20, 3 << 30} {
		if got, err := ParseSize(FormatSize(n)); err != nil || got != n {
			t.Errorf("round trip of %d via %q = %d", n, FormatSize(n), got)
		}
	}
}

func TestParseMirrorURL(t *testing.T) {
	tests := []struct {
		in       string
		host     string
		prefix   string
		insecure bool
	}{
		{"https://docker.arvancloud.ir", "docker.arvancloud.ir", "", false},
		{"mirror.example.com:5000", "mirror.example.com:5000", "", false},
		{"http://10.0.0.5:5000/proxy/", "10.0.0.5:5000", "proxy/", true},
		{"https://harbor.example.com/v2/dockerhub", "harbor.example.com", "dockerhub/", false},
		{"https://Mirror.Example.com/", "mirror.example.com", "", false},
	}
	for _, tt := range tests {
		m, err := ParseMirrorURL(tt.in)
		if err != nil {
			t.Errorf("%s: %v", tt.in, err)
			continue
		}
		if m.Host != tt.host || m.Prefix != tt.prefix || m.Insecure != tt.insecure {
			t.Errorf("%s: got %+v", tt.in, m)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://user:pw@x", "https://x?a=b"} {
		if _, err := ParseMirrorURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	m, _ := ParseMirrorURL("https://registry-1.docker.io")
	if !m.IsUpstream("docker.io") {
		t.Error("registry-1.docker.io should count as Docker Hub itself")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
