package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/pull"
	"github.com/Hkarimi561/ddm/internal/ui"
)

func newPullCmd(g *globals) *cobra.Command {
	opts := pull.Options{Engine: engine.DefaultOptions()}
	var minSpeed, startAt string
	cmd := &cobra.Command{
		Use:   "pull IMAGE",
		Short: "Download an image with resumable transfers and load it into Docker",
		Example: `  ddm pull nginx:latest
  ddm pull --platform linux/arm64 ghcr.io/org/app:1.2
  ddm pull -c 8 --no-load postgres:16`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := g.logger()
			p, conf, err := g.puller(os.Stdout, log)
			if err != nil {
				return err
			}
			// Flags win over the config file, which wins over the defaults.
			f := cmd.Flags()
			if !f.Changed("connections") {
				opts.Engine.Connections = conf.Engine.Connections
			}
			if !f.Changed("parallel") {
				opts.ParallelBlobs = conf.ParallelBlobs
			}
			if !f.Changed("retries") {
				opts.Engine.MaxRetries = conf.Engine.MaxRetries
			}
			if !f.Changed("stall-timeout") {
				opts.Engine.StallTimeout = conf.Engine.StallTimeout
			}
			opts.Engine.MinSpeed = conf.Engine.MinSpeed
			if minSpeed != "" {
				if opts.Engine.MinSpeed, err = config.ParseSize(minSpeed); err != nil {
					return fmt.Errorf("--min-speed: %w", err)
				}
			}
			opts.Engine.Limiter, opts.Engine.Gate = conf.Engine.Limiter, conf.Engine.Gate
			p.Registry.MaxRetries = opts.Engine.MaxRetries

			if !opts.NoLoad {
				// Check Docker before downloading anything, so we don't fetch
				// gigabytes only to find there is nowhere to put them.
				d, err := docker.Find()
				if err != nil {
					return fmt.Errorf("%w\n(use --no-load to only download into the ddm cache)", err)
				}
				p.Docker = d
			}
			if err := waitUntil(cmd.Context(), startAt); err != nil {
				return err
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
	f.StringVar(&startAt, "start-at", "", "wait until this time before downloading, e.g. 02:00 (for cheap night-time bandwidth)")
	return cmd
}

// puller builds a Puller from the configuration, writing messages to out,
// and returns the options the configuration implies.
func (g *globals) puller(out io.Writer, log *slog.Logger) (*pull.Puller, pull.Options, error) {
	cfg, err := g.config()
	if err != nil {
		return nil, pull.Options{}, err
	}
	conf := cfg.Config
	opts := pull.Options{ParallelBlobs: conf.Parallel, Engine: engine.DefaultOptions()}
	opts.Engine.Connections = conf.Connections
	opts.Engine.MaxRetries = conf.Retries
	opts.Engine.StallTimeout = conf.StallTimeout
	opts.Engine.MinSpeed = conf.MinSpeed
	if opts.Engine.Limiter, opts.Engine.Gate, err = g.control(conf.RateLimit); err != nil {
		return nil, opts, err
	}

	c, err := g.cache()
	if err != nil {
		return nil, opts, fmt.Errorf("opening cache: %w", err)
	}
	rc, err := g.registryClient(log)
	if err != nil {
		return nil, opts, err
	}
	p := &pull.Puller{
		Cache:    c,
		Registry: rc,
		Out:      out,
		Log:      log,
		NewReporter: func(total int64) pull.Reporter {
			rep := ui.NewReporter(os.Stderr, total, g.verbose)
			if g.run != nil {
				return g.run.Tee(rep) // also feeds `ddm status`
			}
			return rep
		},
	}
	return p, opts, nil
}

// waitUntil sleeps until the local time given as "HH:MM" (today, or
// tomorrow if that has passed), or until an RFC 3339 timestamp.
func waitUntil(ctx context.Context, at string) error {
	if at == "" {
		return nil
	}
	now := time.Now()
	var when time.Time
	if t, err := time.ParseInLocation("15:04", at, time.Local); err == nil {
		when = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if !when.After(now) {
			when = when.Add(24 * time.Hour)
		}
	} else if t, err := time.Parse(time.RFC3339, at); err == nil {
		when = t
	} else {
		return fmt.Errorf("--start-at: want a time like 02:00 or 2026-11-01T02:00:00+03:30")
	}
	if !when.After(now) {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Waiting until %s to start (%s from now). Ctrl+C to cancel.\n",
		when.Format("Mon 15:04"), when.Sub(now).Round(time.Minute))
	t := time.NewTimer(when.Sub(now))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
