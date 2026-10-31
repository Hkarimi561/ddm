package dockercmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		cmd    string
		kind   Kind
		images []Image
		global []string
	}{
		{"ps -a", Other, nil, nil},
		{"", Other, nil, nil},
		{"run nginx", Run, []Image{{Ref: "nginx"}}, nil},
		{"run -it --rm ubuntu:24.04 bash -c 'echo hi'", Run, []Image{{Ref: "ubuntu:24.04"}}, nil},
		{"run -d -p 8080:80 -v /data:/data --name web nginx:alpine", Run, []Image{{Ref: "nginx:alpine"}}, nil},
		{"run -dp 8080:80 nginx", Run, []Image{{Ref: "nginx"}}, nil},
		{"run -p8080:80 -eFOO=bar nginx", Run, []Image{{Ref: "nginx"}}, nil},
		{"run --platform linux/arm64 alpine uname -m", Run, []Image{{Ref: "alpine", Platform: "linux/arm64"}}, nil},
		{"run --platform=linux/arm64 --rm alpine", Run, []Image{{Ref: "alpine", Platform: "linux/arm64"}}, nil},
		{"run --env-file .env --network host ghcr.io/org/app:1 --flag-for-app", Run, []Image{{Ref: "ghcr.io/org/app:1"}}, nil},
		{"run --pull never nginx", Other, nil, nil},
		{"run --pull=never nginx", Other, nil, nil},
		{"run --pull always nginx", Run, []Image{{Ref: "nginx"}}, nil},
		{"run --sig-proxy=false -i alpine cat", Run, []Image{{Ref: "alpine"}}, nil},
		{"run --rm", Other, nil, nil},
		{"create redis:7", Run, []Image{{Ref: "redis:7"}}, nil},
		{"container run -t busybox", Run, []Image{{Ref: "busybox"}}, nil},
		{"container create busybox", Run, []Image{{Ref: "busybox"}}, nil},
		{"container ls", Other, nil, nil},
		{"pull nginx", Pull, []Image{{Ref: "nginx"}}, nil},
		{"pull -q --platform linux/arm64 nginx:1.27", Pull, []Image{{Ref: "nginx:1.27", Platform: "linux/arm64"}}, nil},
		{"image pull nginx", Pull, []Image{{Ref: "nginx"}}, nil},
		{"pull --all-tags nginx", Other, nil, nil},
		{"--context remote run nginx", Run, []Image{{Ref: "nginx"}}, []string{"--context", "remote"}},
		{"-H tcp://10.0.0.2:2375 --debug pull redis", Pull, []Image{{Ref: "redis"}}, []string{"-H", "tcp://10.0.0.2:2375", "--debug"}},
		{"--config=/tmp/cfg -c dev run alpine", Run, []Image{{Ref: "alpine"}}, []string{"--config=/tmp/cfg", "-c", "dev"}},
		{"model ls", Other, nil, nil},
		{"model run", Other, nil, nil},
	}
	for _, tt := range tests {
		got := Parse(strings.Fields(tt.cmd))
		if got.Kind != tt.kind || !reflect.DeepEqual(got.Images, tt.images) || !reflect.DeepEqual(got.Global, tt.global) {
			t.Errorf("Parse(%q) = kind %v images %v global %v; want kind %v images %v global %v",
				tt.cmd, got.Kind, got.Images, got.Global, tt.kind, tt.images, tt.global)
		}
	}
}

func TestParseModel(t *testing.T) {
	tests := []struct {
		cmd   string
		kind  Kind
		model string
	}{
		{"model run ai/smollm2", ModelRun, "ai/smollm2"},
		{"model run ai/smollm2 Tell me a joke", ModelRun, "ai/smollm2"},
		{"model run -d --debug hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M", ModelRun, "hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M"},
		{"model run --backend llama.cpp gemma3 hi", ModelRun, "gemma3"},
		{"model pull ai/gemma3", ModelPull, "ai/gemma3"},
		{"model pull --ignore-runtime-memory-check ai/gemma3", ModelPull, "ai/gemma3"},
		{"--context desk model pull ai/gemma3", ModelPull, "ai/gemma3"},
		{"model rm ai/gemma3", Other, ""},
	}
	for _, tt := range tests {
		got := Parse(strings.Fields(tt.cmd))
		if got.Kind != tt.kind || got.Model != tt.model {
			t.Errorf("Parse(%q) = kind %v model %q; want %v %q", tt.cmd, got.Kind, got.Model, tt.kind, tt.model)
		}
	}
}

func TestParseCompose(t *testing.T) {
	tests := []struct {
		cmd      string
		kind     Kind
		options  []string
		services []string
		noDeps   bool
	}{
		{"compose up", ComposeUp, nil, nil, false},
		{"compose up -d", ComposeUp, nil, nil, false},
		{"compose -f a.yml -f b.yml --profile debug up -d web", ComposeUp, []string{"-f", "a.yml", "-f", "b.yml", "--profile", "debug"}, []string{"web"}, false},
		{"compose --file=a.yml -p proj up --scale web=3 --no-deps web worker", ComposeUp, []string{"--file=a.yml", "-p", "proj"}, []string{"web", "worker"}, true},
		{"compose up --exit-code-from tests -t 5 tests", ComposeUp, nil, []string{"tests"}, false},
		{"compose create db", ComposeUp, nil, []string{"db"}, false},
		{"compose run --rm -e A=1 web python manage.py migrate", ComposeUp, nil, []string{"web"}, false},
		{"compose run --no-deps -T web sh", ComposeUp, nil, []string{"web"}, true},
		{"compose pull", ComposePull, nil, nil, false},
		{"compose pull --policy missing db cache", ComposePull, nil, []string{"db", "cache"}, false},
		{"compose up --pull never", Other, nil, nil, false},
		{"compose ps", Other, nil, nil, false},
		{"compose down -v", Other, nil, nil, false},
		{"compose run", Other, nil, nil, false},
	}
	for _, tt := range tests {
		got := Parse(strings.Fields(tt.cmd))
		if got.Kind != tt.kind {
			t.Errorf("Parse(%q) kind = %v, want %v", tt.cmd, got.Kind, tt.kind)
			continue
		}
		if tt.kind == Other {
			continue
		}
		c := got.Compose
		if !reflect.DeepEqual(c.Options, tt.options) || !reflect.DeepEqual(c.Services, tt.services) || c.NoDeps != tt.noDeps {
			t.Errorf("Parse(%q) = %+v; want options %v services %v noDeps %v", tt.cmd, *c, tt.options, tt.services, tt.noDeps)
		}
	}
}

const composeJSON = `{
  "name": "shop",
  "services": {
    "web": {"image": "ghcr.io/shop/web:2", "depends_on": {"api": {"condition": "service_started"}}},
    "api": {"image": "ghcr.io/shop/api:2", "depends_on": {"db": {"condition": "service_healthy"}, "cache": {}}},
    "db": {"image": "postgres:16", "platform": "linux/amd64"},
    "cache": {"image": "redis:7"},
    "worker": {"image": "ghcr.io/shop/api:2"},
    "builder": {"build": {"context": "."}, "image": "shop/builder:dev"},
    "pinned": {"build": {"context": "."}, "image": "shop/pinned:1", "pull_policy": "always"},
    "offline": {"image": "local/thing", "pull_policy": "never"},
    "noimage": {"build": {"context": "./x"}}
  }
}`

func TestComposeImages(t *testing.T) {
	refs := func(imgs []Image) []string {
		var out []string
		for _, i := range imgs {
			out = append(out, i.Ref)
		}
		return out
	}
	all, err := ComposeImages([]byte(composeJSON), &Compose{})
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by service name; api's image is shared with worker and listed once.
	if want := []string{"ghcr.io/shop/api:2", "redis:7", "postgres:16", "shop/pinned:1", "ghcr.io/shop/web:2"}; !reflect.DeepEqual(refs(all), want) {
		t.Errorf("all services: %v, want %v", refs(all), want)
	}

	web, err := ComposeImages([]byte(composeJSON), &Compose{Services: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ghcr.io/shop/api:2", "redis:7", "postgres:16", "ghcr.io/shop/web:2"}; !reflect.DeepEqual(refs(web), want) {
		t.Errorf("web with dependencies: %v, want %v", refs(web), want)
	}
	for _, img := range web {
		if img.Ref == "postgres:16" && img.Platform != "linux/amd64" {
			t.Errorf("service platform lost: %+v", img)
		}
	}

	only, _ := ComposeImages([]byte(composeJSON), &Compose{Services: []string{"web"}, NoDeps: true})
	if want := []string{"ghcr.io/shop/web:2"}; !reflect.DeepEqual(refs(only), want) {
		t.Errorf("--no-deps: %v, want %v", refs(only), want)
	}

	if _, err := ComposeImages([]byte(composeJSON), &Compose{Services: []string{"nope"}}); err == nil {
		t.Error("unknown service accepted")
	}
	if _, err := ComposeImages([]byte("not json"), &Compose{}); err == nil {
		t.Error("bad JSON accepted")
	}
}
