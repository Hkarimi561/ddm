package modelrunner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Hkarimi561/ddm/internal/docker"
)

const (
	desktopBase = "http://localhost/exp/vDD4.40" // through the Docker engine socket
	tcpBase     = "http://localhost:12434"       // Docker Engine's runner container, or Desktop with host TCP enabled
)

// Detect finds the runner the same way `docker model` does:
// MODEL_RUNNER_HOST if set; on Docker Desktop, the engine socket; otherwise
// (or as a fallback) localhost:12434.
func Detect(ctx context.Context, cli *docker.CLI) (*Client, error) {
	if h := os.Getenv("MODEL_RUNNER_HOST"); h != "" {
		if !strings.Contains(h, "://") {
			h = "http://" + h
		}
		c := &Client{Kind: KindManual, Base: strings.TrimRight(h, "/"), HTTP: &http.Client{}}
		if err := ping(ctx, c); err != nil {
			return nil, fmt.Errorf("Docker Model Runner at %s (MODEL_RUNNER_HOST) is not reachable: %w", c.Base, err)
		}
		return c, nil
	}

	info, err := cli.Info(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []*Client
	tcpKind := KindEngine
	if IsDesktop(info) {
		tcpKind = KindDesktop
		host, err := cli.Output(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err == nil {
			if t, err := socketTransport(host); err == nil {
				candidates = append(candidates, &Client{Kind: KindDesktop, Base: desktopBase, HTTP: &http.Client{Transport: t}})
			}
		}
	}
	candidates = append(candidates, &Client{Kind: tcpKind, Base: tcpBase, HTTP: &http.Client{}})

	var tried []string
	for _, c := range candidates {
		if err := ping(ctx, c); err == nil {
			return c, nil
		}
		tried = append(tried, c.Base)
	}
	return nil, fmt.Errorf("Docker Model Runner is not reachable (tried %s)", strings.Join(tried, ", "))
}

func ping(ctx context.Context, c *Client) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.List(pctx)
	return err
}

// IsDesktop reports whether the engine is Docker Desktop's. Desktop on
// Linux (outside WSL2) runs the runner as a container like Docker Engine
// does, so it doesn't count.
func IsDesktop(info *docker.Info) bool {
	if info == nil || !strings.Contains(info.OperatingSystem, "Docker Desktop") {
		return false
	}
	return runtime.GOOS != "linux" || isWSL()
}

func isWSL() bool {
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// socketTransport dials the Docker engine endpoint of the current context.
func socketTransport(host string) (http.RoundTripper, error) {
	host = strings.TrimSpace(host)
	var dial func(ctx context.Context) (net.Conn, error)
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		dial = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	case strings.HasPrefix(host, "npipe://"):
		path := strings.ReplaceAll(strings.TrimPrefix(host, "npipe://"), "/", `\`)
		dial = func(ctx context.Context) (net.Conn, error) { return dialPipe(ctx, path) }
	default:
		return nil, errors.New("unsupported Docker endpoint " + host)
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}, nil
}

// InstallHint tells how to get the `docker model` plugin.
func InstallHint() string {
	if runtime.GOOS == "linux" {
		return "Install the plugin: `sudo apt-get install docker-model-plugin` (Debian/Ubuntu) or `sudo dnf install docker-model-plugin` (Fedora/RHEL), or use Docker Desktop."
	}
	return "Update Docker Desktop (4.40 or later includes Docker Model Runner)."
}

// EnableHint tells how to turn the runner on.
func EnableHint(desktop bool) string {
	if desktop {
		return "Enable it in Docker Desktop under Settings > AI > Enable Docker Model Runner, or run `docker desktop enable model-runner`."
	}
	return "Start it with `docker model install-runner` (see `docker model install-runner --help` for GPU options)."
}

// EnableCommand returns the docker command that turns the runner on.
func EnableCommand(desktop bool) []string {
	if desktop {
		return []string{"desktop", "enable", "model-runner"}
	}
	return []string{"model", "install-runner"}
}
