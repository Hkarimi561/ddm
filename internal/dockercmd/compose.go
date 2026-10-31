package dockercmd

import (
	"encoding/json"
	"fmt"
	"sort"
)

// composeModel is the part of `docker compose config --format json` ddm
// needs. Compose has already applied -f, profiles, .env interpolation and
// extends by then, so ddm never parses compose files itself.
type composeModel struct {
	Services map[string]composeService `json:"services"`
}

type composeService struct {
	Image      string                     `json:"image"`
	Build      json.RawMessage            `json:"build"`
	PullPolicy string                     `json:"pull_policy"`
	Platform   string                     `json:"platform"`
	DependsOn  map[string]json.RawMessage `json:"depends_on"`
}

// ComposeImages returns the images that c needs, given the project's
// resolved configuration. Services that are built locally or never pulled
// are skipped; so are images of services that won't be started.
func ComposeImages(configJSON []byte, c *Compose) ([]Image, error) {
	var m composeModel
	if err := json.Unmarshal(configJSON, &m); err != nil {
		return nil, fmt.Errorf("reading compose configuration: %w", err)
	}

	selected := map[string]bool{}
	if len(c.Services) == 0 {
		for name := range m.Services {
			selected[name] = true
		}
	} else {
		var visit func(string)
		visit = func(name string) {
			svc, ok := m.Services[name]
			if !ok || selected[name] {
				return
			}
			selected[name] = true
			if c.NoDeps {
				return
			}
			for dep := range svc.DependsOn {
				visit(dep)
			}
		}
		for _, name := range c.Services {
			if _, ok := m.Services[name]; !ok {
				return nil, fmt.Errorf("no such service in the compose project: %s", name)
			}
			visit(name)
		}
	}

	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[Image]bool{}
	var out []Image
	for _, name := range names {
		svc := m.Services[name]
		hasBuild := len(svc.Build) > 0 && string(svc.Build) != "null"
		switch {
		case svc.Image == "":
			continue
		case svc.PullPolicy == "never" || svc.PullPolicy == "build":
			continue
		case hasBuild && svc.PullPolicy != "always" && svc.PullPolicy != "missing":
			continue // compose builds it rather than pulling it
		}
		img := Image{Ref: svc.Image, Platform: svc.Platform}
		if !seen[img] {
			seen[img] = true
			out = append(out, img)
		}
	}
	return out, nil
}
