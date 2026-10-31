package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/diskspace"
	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/ui"
)

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusInfo checkStatus = "info"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

type check struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
	Fix    string      `json:"fix,omitempty"`
}

const lowDiskSpace = 5 << 30

func newDoctorCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check Docker, the cache, the config and registry access",
		Long: `Checks everything ddm depends on and explains how to fix what is wrong:
the Docker CLI and engine, the compose and model plugins, the config file,
the cache and its free space, the proxy, and whether each registry endpoint
(mirrors included) answers. Exits with status 1 if something is broken.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runDoctor(cmd.Context(), g)
			if asJSON {
				if err := printJSON(checks); err != nil {
					return err
				}
			} else {
				printChecks(checks)
			}
			for _, c := range checks {
				if c.Status == statusFail {
					return &exitError{code: 1}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printChecks(checks []check) {
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	fmt.Println()
	for _, c := range checks {
		fmt.Printf("  %-4s  %-*s  %s\n", strings.ToUpper(string(c.Status)), width, c.Name, c.Detail)
		if c.Fix != "" {
			for _, line := range strings.Split(c.Fix, "\n") {
				fmt.Printf("        %-*s  -> %s\n", width, "", line)
			}
		}
	}
	fmt.Println()
}

func runDoctor(ctx context.Context, g *globals) []check {
	var checks []check
	add := func(c check) { checks = append(checks, c) }

	add(check{"ddm", statusOK, fmt.Sprintf("%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH), ""})

	// Config.
	cfg, cfgErr := g.config()
	switch {
	case cfgErr != nil:
		add(check{"config", statusFail, cfgErr.Error(), "Fix the file, or move it away to start from the defaults."})
	case fileExists(cfg.Path):
		add(check{"config", statusOK, cfg.Path, ""})
	default:
		add(check{"config", statusInfo, cfg.Path + " (not created yet; using defaults)", ""})
	}

	// Cache.
	add(cacheCheck(g))

	// Docker.
	cli, err := docker.Find()
	if err != nil {
		add(check{"docker CLI", statusFail, "not found in PATH", docker.InstallHint()})
		add(check{"docker engine", statusFail, "skipped: no docker CLI", ""})
	} else {
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		info, ierr := cli.Info(ictx)
		cancel()
		detail := cli.Bin
		if info != nil && info.ClientInfo.Version != "" {
			detail += fmt.Sprintf(" (version %s, context %s)", info.ClientInfo.Version, orDefault(info.ClientInfo.Context, "default"))
		}
		add(check{"docker CLI", statusOK, detail, ""})
		if ierr != nil {
			add(check{"docker engine", statusFail, "not reachable: " + firstLine(ierr.Error()), docker.StartHint(ierr.Error())})
		} else {
			store := "classic image store"
			if info.ContainerdStore() {
				store = "containerd image store"
			}
			add(check{"docker engine", statusOK, fmt.Sprintf("%s on %s/%s, %s (%s)",
				info.ServerVersion, info.OSType, info.Architecture, info.OperatingSystem, store), ""})
		}
		if info != nil {
			add(pluginCheck(info, "compose", statusWarn,
				"`ddm docker compose ...` needs it.",
				composeHint()))
			add(pluginCheck(info, "model", statusInfo,
				"needed for Docker models (`docker model ...`).",
				"Enable Docker Model Runner in Docker Desktop (Settings > AI), or install the docker-model-plugin package on Linux."))
		}
	}

	// Proxy and registries.
	if cfgErr == nil {
		add(proxyCheck(cfg.Config))
		checks = append(checks, registryChecks(ctx, g, cfg)...)
	}
	return checks
}

func cacheCheck(g *globals) check {
	c, err := g.cache()
	if err != nil {
		return check{"cache", statusFail, err.Error(), "Pick a writable directory with `ddm config set cache_dir <dir>`."}
	}
	probe, err := os.CreateTemp(filepath.Join(c.Root, "partial"), "doctor-*")
	if err != nil {
		return check{"cache", statusFail, c.Root + " is not writable: " + err.Error(), "Pick a writable directory with `ddm config set cache_dir <dir>`."}
	}
	probe.Close()
	os.Remove(probe.Name())

	used := dirSize(filepath.Join(c.Root, "blobs"))
	detail := fmt.Sprintf("%s (%s cached", c.Root, ui.HumanBytes(used))
	free, err := diskspace.Free(c.Root)
	if err != nil {
		return check{"cache", statusOK, detail + ")", ""}
	}
	detail += fmt.Sprintf(", %s free)", ui.HumanBytes(int64(free)))
	if free < lowDiskSpace {
		return check{"cache", statusWarn, detail, "Free up disk space, or move the cache with `ddm config set cache_dir <dir>`."}
	}
	return check{"cache", statusOK, detail, ""}
}

func pluginCheck(info *docker.Info, plugin string, missing checkStatus, why, fix string) check {
	name := "docker " + plugin
	if p, ok := info.Plugin(plugin); ok {
		return check{name, statusOK, orDefault(p.Version, "installed"), ""}
	}
	return check{name, missing, "not installed: " + why, fix}
}

func composeHint() string {
	if runtime.GOOS == "linux" {
		return "Install the docker-compose-plugin package (https://docs.docker.com/compose/install/linux/)."
	}
	return "Docker Desktop includes it; update Docker Desktop."
}

func proxyCheck(c config.Config) check {
	switch {
	case c.Proxy != "":
		return check{"proxy", statusInfo, c.Proxy + " (from config)", ""}
	case os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "":
		return check{"proxy", statusInfo, orDefault(os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy")) + " (from HTTPS_PROXY)", ""}
	}
	return check{"proxy", statusInfo, "none", ""}
}

// registryChecks pings every endpoint of docker.io and of each registry with
// mirrors. A registry is broken only if none of its endpoints answer.
func registryChecks(ctx context.Context, g *globals, cfg *config.File) []check {
	rc, err := g.registryClient(g.logger())
	if err != nil {
		return []check{{"registries", statusFail, err.Error(), ""}}
	}
	regs := []string{"docker.io"}
	for _, r := range cfg.MirrorRegistries() {
		if config.NormalizeRegistry(r) != config.DockerHub {
			regs = append(regs, r)
		}
	}

	type result struct {
		rtt time.Duration
		err error
	}
	var out []check
	for _, r := range regs {
		reg, err := name.NewRegistry(config.NormalizeRegistry(r))
		if err != nil {
			out = append(out, check{r, statusFail, err.Error(), ""})
			continue
		}
		eps := rc.Endpoints(reg)
		results := make([]result, len(eps))
		var wg sync.WaitGroup
		for i, ep := range eps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				rtt, err := rc.Ping(pctx, ep)
				results[i] = result{rtt, err}
			}()
		}
		wg.Wait()

		reachable := 0
		for _, res := range results {
			if res.err == nil {
				reachable++
			}
		}
		for i, ep := range eps {
			label := ep.Name
			if ep.Upstream {
				label += " (upstream)"
			}
			res := results[i]
			switch {
			case res.err == nil:
				out = append(out, check{label, statusOK, fmt.Sprintf("reachable, %s", res.rtt.Round(time.Millisecond)), ""})
			case reachable > 0:
				out = append(out, check{label, statusWarn, "unreachable: " + firstLine(res.err.Error()), "Other endpoints work, so pulls can still succeed."})
			default:
				out = append(out, check{label, statusFail, "unreachable: " + firstLine(res.err.Error()), registryHint(r)})
			}
		}
	}
	return out
}

func registryHint(reg string) string {
	return fmt.Sprintf("Check your network or proxy (`ddm config set proxy ...`), or add a mirror: `ddm mirror add %s <url>`.", reg)
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
