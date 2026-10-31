// Package cli wires ddm's commands together.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/registry"
)

// Version is reported by `ddm --version`.
var Version = "dev"

type globals struct {
	verbose    bool
	cacheDir   string
	configPath string

	cfg *config.File
}

// Execute runs ddm and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first Ctrl+C we shut down gracefully (saving download state);
	// restoring the default handler lets a second Ctrl+C kill us immediately.
	go func() {
		<-ctx.Done()
		stop()
	}()

	err := newRootCmd().ExecuteContext(ctx)
	var exit *exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "\nInterrupted. Download progress is saved; run the same command again to resume.")
		return 130
	default:
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
}

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "ddm",
		Short: "Resumable, segmented downloads for Docker images",
		Long: `ddm downloads Docker images directly from registries using parallel,
resumable HTTP range requests, verifies every blob, and then loads the
result into your local Docker. Interrupted downloads continue from where
they stopped, even after a reboot.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&g.verbose, "verbose", false, "print debug logs")
	pf.StringVar(&g.cacheDir, "cache-dir", "", "blob cache directory (default: $DDM_CACHE_DIR, cache_dir from the config, or the OS cache directory)")
	pf.StringVar(&g.configPath, "config", "", "config file (default: $DDM_CONFIG or the OS config directory)")

	root.AddCommand(newPullCmd(g), newDockerCmd(g), newDoctorCmd(g), newShellInitCmd(), newConfigCmd(g), newMirrorCmd(g))
	return root
}

func (g *globals) logger() *slog.Logger {
	level := slog.LevelWarn
	if g.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && !g.verbose {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// config loads the configuration file once.
func (g *globals) config() (*config.File, error) {
	if g.cfg != nil {
		return g.cfg, nil
	}
	path := g.configPath
	if path == "" {
		var err error
		if path, err = config.Path(); err != nil {
			return nil, err
		}
	}
	f, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	g.cfg = f
	return f, nil
}

// cache opens the blob cache. Its location comes from --cache-dir, then
// $DDM_CACHE_DIR, then cache_dir in the config, then the OS default.
func (g *globals) cache() (*cache.Cache, error) {
	root := g.cacheDir
	if root == "" {
		root = os.Getenv("DDM_CACHE_DIR")
	}
	if root == "" {
		cfg, err := g.config()
		if err != nil {
			return nil, err
		}
		root = cfg.Config.CacheDir
	}
	if root == "" {
		var err error
		if root, err = cache.DefaultRoot(); err != nil {
			return nil, err
		}
	}
	return cache.New(root)
}

// registryClient builds a registry client with the configured proxy and
// mirrors.
func (g *globals) registryClient(log *slog.Logger) (*registry.Client, error) {
	cfg, err := g.config()
	if err != nil {
		return nil, err
	}
	c := cfg.Config
	t := registry.NewTransport(registry.ProxyFunc(c.Proxy, c.NoProxy))
	rc := registry.NewClient(t, c.Mirrors, log)
	rc.MaxRetries = c.Retries
	return rc, nil
}
