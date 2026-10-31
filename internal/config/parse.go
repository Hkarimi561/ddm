package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DockerHub is the canonical name of Docker Hub's registry, as used by
// go-containerregistry.
const DockerHub = "index.docker.io"

// NormalizeRegistry turns the ways people write a registry ("docker.io",
// "https://registry-1.docker.io/", "GHCR.io") into one comparable form.
func NormalizeRegistry(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimSuffix(s, "/")
	switch s {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "hub.docker.com":
		return DockerHub
	}
	return s
}

// Mirror is a parsed mirror URL.
type Mirror struct {
	Raw      string // as configured
	Insecure bool   // plain http
	Host     string // host[:port]
	// Prefix is prepended to repository paths, for registries that serve a
	// proxy cache under a project, e.g. Harbor's https://harbor.example.com/dockerhub
	// serves library/nginx as dockerhub/library/nginx.
	Prefix string
}

// ParseMirrorURL parses a mirror such as "https://docker.arvancloud.ir",
// "mirror.example.com:5000" or "http://10.0.0.5:5000/proxy".
func ParseMirrorURL(s string) (Mirror, error) {
	raw := strings.TrimSpace(s)
	withScheme := raw
	if !strings.Contains(raw, "://") {
		withScheme = "https://" + raw
	}
	u, err := url.Parse(withScheme)
	if err != nil || u.Host == "" {
		return Mirror{}, fmt.Errorf("invalid mirror URL %q", s)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Mirror{}, fmt.Errorf("mirror %q: only http:// and https:// are supported", s)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return Mirror{}, fmt.Errorf("mirror %q: must not contain credentials, a query or a fragment (credentials come from `docker login`)", s)
	}
	prefix := strings.Trim(u.Path, "/")
	prefix = strings.TrimPrefix(strings.TrimPrefix(prefix, "v2"), "/")
	if prefix != "" {
		prefix += "/"
	}
	return Mirror{Raw: raw, Insecure: u.Scheme == "http", Host: strings.ToLower(u.Host), Prefix: prefix}, nil
}

// IsUpstream reports whether the mirror URL points at the upstream registry
// itself (no prefix).
func (m Mirror) IsUpstream(registry string) bool {
	return m.Prefix == "" && NormalizeRegistry(m.Host) == NormalizeRegistry(registry)
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30},
	{"kb", 1000}, {"mb", 1000 * 1000}, {"gb", 1000 * 1000 * 1000},
	{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30},
	{"b", 1},
}

// ParseSize parses sizes like "512", "32KiB", "1.5MB", "2M" (bare K/M/G are
// binary). A trailing "/s" is accepted, so speeds read naturally.
func ParseSize(s string) (int64, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.TrimSuffix(v, "/s")
	mult := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(v, u.suffix) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q (examples: 0, 512KiB, 2MiB)", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatSize formats n with the largest exact-ish binary unit.
func FormatSize(n int64) string {
	switch {
	case n == 0:
		return "0"
	case n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return strconv.FormatInt(n, 10)
}
