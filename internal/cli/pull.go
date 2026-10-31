package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/pull"
	"github.com/Hkarimi561/ddm/internal/ui"
)

func newPullCmd(g *globals) *cobra.Command {
	opts := pull.Options{Engine: engine.DefaultOptions()}
	var minSpeed string
	cmd := &cobra.Command{
		Use:   "pull IMAGE",
		Short: "Download an image with resumable transfers and load it into Docker",
		Example: `  ddm pull nginx:latest
  ddm pull --platform linux/arm64 ghcr.io/org/app:1.2
  ddm pull -c 8 --no-load postgres:16`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := g.logger()
			cfg, err := g.config()
			if err != nil {
				return err
			}
			// Flags win over the config file, which wins over the defaults.
			f, conf := cmd.Flags(), cfg.Config
			if !f.Changed("connections") {
				opts.Engine.Connections = conf.Connections
			}
			if !f.Changed("parallel") {
				opts.ParallelBlobs = conf.Parallel
			}
			if !f.Changed("retries") {
				opts.Engine.MaxRetries = conf.Retries
			}
			if !f.Changed("stall-timeout") {
				opts.Engine.StallTimeout = conf.StallTimeout
			}
			opts.Engine.MinSpeed = conf.MinSpeed
			if minSpeed != "" {
				if opts.Engine.MinSpeed, err = config.ParseSize(minSpeed); err != nil {
					return fmt.Errorf("--min-speed: %w", err)
				}
			}

			c, err := g.cache()
			if err != nil {
				return fmt.Errorf("opening cache: %w", err)
			}
			rc, err := g.registryClient(log)
			if err != nil {
				return err
			}
			rc.MaxRetries = opts.Engine.MaxRetries
			p := &pull.Puller{
				Cache:    c,
				Registry: rc,
				Out:      os.Stdout,
				Log:      log,
				NewReporter: func(total int64) pull.Reporter {
					return ui.NewReporter(os.Stderr, total, g.verbose)
				},
			}
			if !opts.NoLoad {
				// Check Docker before downloading anything, so we don't fetch
				// gigabytes only to find there is nowhere to put them.
				d, err := docker.Find()
				if err != nil {
					return fmt.Errorf("%w\n(use --no-load to only download into the ddm cache)", err)
				}
				p.Docker = d
			}
			return p.Pull(cmd.Context(), args[0], opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Platform, "platform", "", "platform to pull, e.g. linux/arm64 (default: the Docker daemon's platform)")
	f.IntVarP(&opts.Engine.Connections, "connections", "c", opts.Engine.Connections, "parallel connections per blob")
	f.IntVarP(&opts.ParallelBlobs, "parallel", "p", 3, "number of blobs downloaded at the same time")
	f.IntVar(&opts.Engine.MaxRetries, "retries", 0, "consecutive failed attempts before giving up (0 = retry forever)")
	f.DurationVar(&opts.Engine.StallTimeout, "stall-timeout", opts.Engine.StallTimeout, "reconnect when no data arrives for this long")
	f.BoolVar(&opts.NoLoad, "no-load", false, "only download into the ddm cache; don't load into Docker")
	f.StringVar(&minSpeed, "min-speed", "", "switch to the next mirror below this speed, e.g. 64KiB (0 = never; default from config)")
	return cmd
}
