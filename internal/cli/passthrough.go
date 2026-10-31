package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/dockercmd"
	"github.com/Hkarimi561/ddm/internal/pull"
)

func newDockerCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "docker [DOCKER ARGS...]",
		Short: "Run a docker command, downloading the images it needs with ddm first",
		Long: `Runs any docker command. For commands that need images (docker run,
create, pull, and docker compose up, create, run, pull), ddm first downloads
the images that are missing, with resumable transfers and your mirrors, then
runs the original command unchanged. Everything else goes straight to docker.

ddm's own messages go to stderr, so the command's output is untouched. Set
DDM_PASSTHROUGH=off to skip ddm entirely. With "ddm shell-init", plain
"docker" commands go through ddm too.`,
		Example: `  ddm docker run --rm -it ubuntu:24.04 bash
  ddm docker compose -f prod.yml --profile web up -d
  ddm docker pull --platform linux/arm64 nginx`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return passThrough(cmd.Context(), g, args)
		},
	}
}

// exitError ends ddm with a given exit code (docker's) without printing.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func passThrough(ctx context.Context, g *globals, args []string) error {
	cli, err := docker.Find()
	if err != nil {
		return err
	}
	if os.Getenv("DDM_PASSTHROUGH") != "off" {
		done, err := prepare(ctx, g, cli, args)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	g.close() // on Unix, Exec replaces this process: unregister first
	code, err := docker.Exec(cli.Bin, args)
	if err != nil {
		return err
	}
	return &exitError{code: code}
}

// prepare downloads the images the command needs. It reports whether ddm
// did the command's whole job (a pull), in which case docker needn't run.
//
// Failing to download an image is not fatal: docker gets to try on its own,
// as it would without ddm. A stopped engine, a full disk and Ctrl+C do stop
// the command.
func prepare(ctx context.Context, g *globals, cli *docker.CLI, args []string) (bool, error) {
	cmd := dockercmd.Parse(args)
	if cmd.Kind == dockercmd.Other {
		return false, nil
	}
	cli.Global = cmd.Global
	if cmd.Kind == dockercmd.ModelRun || cmd.Kind == dockercmd.ModelPull {
		return prepareModel(ctx, g, cli, cmd)
	}
	log := g.logger()

	images := cmd.Images
	if cmd.Compose != nil {
		cfgArgs := append(append([]string{"compose"}, cmd.Compose.Options...), "config", "--format", "json")
		out, err := cli.Output(ctx, cfgArgs...)
		if err == nil {
			images, err = dockercmd.ComposeImages([]byte(out), cmd.Compose)
		}
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			// Compose will report the problem itself, in its own words.
			log.Debug("could not read the compose project", "err", err)
			return false, nil
		}
	}
	if len(images) == 0 {
		return false, nil
	}
	if _, err := cli.ServerPlatform(ctx); err != nil {
		return false, err
	}

	p, opts, err := g.puller(os.Stderr, log)
	if err != nil {
		return false, err
	}
	p.Docker = cli
	// Like docker: run/up only fetch missing images; pull checks for updates.
	update := cmd.Kind == dockercmd.Pull || cmd.Kind == dockercmd.ComposePull
	allDone := true
	for _, img := range images {
		if !update {
			if id, err := cli.ImageID(ctx, img.Ref); err == nil && id != "" {
				continue
			}
		}
		o := opts
		o.Platform = img.Platform
		err := p.Pull(ctx, img.Ref, o)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var noSpace *pull.NoSpaceError
		if errors.As(err, &noSpace) {
			return false, err
		}
		allDone = false
		fmt.Fprintf(os.Stderr, "ddm: could not download %s: %v\nddm: leaving it to docker.\n", img.Ref, err)
	}
	return update && allDone, nil
}

// prepareModel handles `docker model run|pull`: Model Runner must be
// installed and running (ddm explains how, and offers to enable it), then a
// missing model is downloaded and loaded by ddm before docker runs.
func prepareModel(ctx context.Context, g *globals, cli *docker.CLI, cmd dockercmd.Command) (bool, error) {
	mp, err := g.modelPuller(ctx, os.Stderr, true, cli)
	if err != nil {
		return false, err
	}
	if cmd.Kind == dockercmd.ModelRun {
		if ok, err := mp.Present(ctx, cmd.Model); err == nil && ok {
			return false, nil
		}
	}
	err = mp.Pull(ctx, cmd.Model)
	if err == nil {
		return cmd.Kind == dockercmd.ModelPull, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var noSpace *pull.NoSpaceError
	if errors.As(err, &noSpace) {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "ddm: could not download %s: %v\nddm: leaving it to docker.\n", cmd.Model, err)
	return false, nil
}
