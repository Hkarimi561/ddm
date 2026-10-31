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
)

// Version is reported by `ddm --version`.
var Version = "dev"

type globals struct {
	verbose  bool
	cacheDir string
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
	switch {
	case err == nil:
		return 0
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
	root.PersistentFlags().BoolVar(&g.verbose, "verbose", false, "print debug logs")
	root.PersistentFlags().StringVar(&g.cacheDir, "cache-dir", "", "blob cache directory (default: $DDM_CACHE_DIR or the OS cache directory)")

	root.AddCommand(newPullCmd(g))
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

func (g *globals) cache() (*cache.Cache, error) {
	root := g.cacheDir
	if root == "" {
		var err error
		if root, err = cache.DefaultRoot(); err != nil {
			return nil, err
		}
	}
	return cache.New(root)
}
