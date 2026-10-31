package registry

import (
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/Hkarimi561/ddm/internal/config"
)

// Endpoint is a place an upstream registry's content can be fetched from:
// a mirror of it, or the upstream itself.
type Endpoint struct {
	Name     string // for display: the mirror URL as configured, or the upstream registry
	Upstream bool
	registry name.Registry
	prefix   string // repository path prefix on the mirror
}

// Repo returns where upstream's repository lives on this endpoint.
func (e Endpoint) Repo(upstream name.Repository) name.Repository {
	if e.Upstream {
		return upstream
	}
	return e.registry.Repo(e.prefix + upstream.RepositoryStr())
}

// Ref returns ref as found on this endpoint (same tag or digest).
func (e Endpoint) Ref(ref name.Reference) name.Reference {
	if e.Upstream {
		return ref
	}
	repo := e.Repo(ref.Context())
	if d, ok := ref.(name.Digest); ok {
		return repo.Digest(d.DigestStr())
	}
	return repo.Tag(ref.Identifier())
}

// Endpoints lists where content of the upstream registry reg is fetched
// from, in order: the configured mirrors, then the upstream itself, unless
// the mirror list already places the upstream somewhere.
func (c *Client) Endpoints(reg name.Registry) []Endpoint {
	upstream := Endpoint{Name: displayName(reg), Upstream: true, registry: reg}
	want := config.NormalizeRegistry(reg.RegistryStr())
	var out []Endpoint
	listedUpstream := false
	for key, urls := range c.Mirrors {
		if config.NormalizeRegistry(key) != want {
			continue
		}
		for _, u := range urls {
			m, err := config.ParseMirrorURL(u)
			if err != nil {
				continue // rejected when the config was loaded
			}
			if m.IsUpstream(reg.RegistryStr()) {
				if !listedUpstream {
					out = append(out, upstream)
					listedUpstream = true
				}
				continue
			}
			var opts []name.Option
			if m.Insecure {
				opts = append(opts, name.Insecure)
			}
			r, err := name.NewRegistry(m.Host, opts...)
			if err != nil {
				continue
			}
			out = append(out, Endpoint{Name: m.Raw, registry: r, prefix: m.Prefix})
		}
	}
	if !listedUpstream {
		out = append(out, upstream)
	}
	return out
}

func displayName(reg name.Registry) string {
	if reg.RegistryStr() == name.DefaultRegistry {
		return "docker.io"
	}
	return reg.RegistryStr()
}
