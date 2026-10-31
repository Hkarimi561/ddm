package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Hkarimi561/ddm/internal/docker"
	"github.com/Hkarimi561/ddm/internal/hf"
	"github.com/Hkarimi561/ddm/internal/modelpull"
	"github.com/Hkarimi561/ddm/internal/modelrunner"
	"github.com/Hkarimi561/ddm/internal/sysmem"
)

func newModelCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Download Docker Model Runner models with resumable transfers",
	}
	var noLoad bool
	pullCmd := &cobra.Command{
		Use:   "pull MODEL",
		Short: "Download a model and load it into Docker Model Runner",
		Long: `Downloads a model with resumable, verified transfers and loads it into
Docker Model Runner under the same name "docker model pull" would use.

Models from Docker Hub and other registries keep their original ID. Models
from Hugging Face (hf.co/...) are fetched file by file from the Hub (or a
mirror: "ddm mirror add hf.co https://hf-mirror.com") and packaged with
"docker model package". Gated repositories need HF_TOKEN.`,
		Example: `  ddm model pull ai/smollm2
  ddm model pull hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M
  ddm model pull --no-load ai/gemma3     # download only`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mp, err := g.modelPuller(cmd.Context(), os.Stdout, !noLoad, nil)
			if err != nil {
				return err
			}
			return mp.Pull(cmd.Context(), args[0])
		},
	}
	pullCmd.Flags().BoolVar(&noLoad, "no-load", false, "only download into the ddm cache; don't load into Docker Model Runner")
	cmd.AddCommand(pullCmd)
	return cmd
}

// modelPuller builds a model puller. With load, it first makes sure Docker
// Model Runner is installed and running (offering to enable it); cli, if
// given, carries docker's global options from a pass-through command.
func (g *globals) modelPuller(ctx context.Context, out io.Writer, load bool, cli *docker.CLI) (*modelpull.Puller, error) {
	log := g.logger()
	p, opts, err := g.puller(out, log)
	if err != nil {
		return nil, err
	}
	cfg, err := g.config()
	if err != nil {
		return nil, err
	}
	mp := &modelpull.Puller{
		Images: p,
		Opts:   opts,
		Hub:    hf.NewHub(cfg.Config.Mirrors, p.Registry.Transport),
		Out:    out,
		Log:    log,
	}
	if !load {
		return mp, nil
	}
	if cli == nil {
		if cli, err = docker.Find(); err != nil {
			return nil, fmt.Errorf("%w\n(use --no-load to only download into the ddm cache)", err)
		}
	}
	runner, err := g.ensureModelRunner(ctx, cli, out)
	if err != nil {
		return nil, err
	}
	mp.Runner, mp.Docker = runner, cli
	return mp, nil
}

// ensureModelRunner checks, in order, that the engine runs, that the
// `docker model` plugin is installed, and that Model Runner answers. If the
// runner is off it explains how to turn it on and, on a terminal, offers to
// run the enabling command. It never installs packages.
func (g *globals) ensureModelRunner(ctx context.Context, cli *docker.CLI, out io.Writer) (*modelrunner.Client, error) {
	if _, err := cli.ServerPlatform(ctx); err != nil {
		return nil, err
	}
	if _, err := cli.Output(ctx, "model", "version"); err != nil {
		return nil, fmt.Errorf("the Docker Model Runner plugin (`docker model`) is not installed.\n%s", modelrunner.InstallHint())
	}
	if r, err := modelrunner.Detect(ctx, cli); err == nil {
		return r, nil
	}

	info, _ := cli.Info(ctx)
	desktop := modelrunner.IsDesktop(info)
	enable := modelrunner.EnableCommand(desktop)
	fmt.Fprintf(os.Stderr, "Docker Model Runner is not running.\n%s\n", modelrunner.EnableHint(desktop))
	if !confirm(fmt.Sprintf("Run `docker %s` now?", strings.Join(enable, " "))) {
		return nil, &exitError{code: 1}
	}
	if !desktop && sysmem.GPUMemory() == 0 {
		// install-runner pulls the runner image; fetch it with ddm first so
		// a bad connection can't break that step.
		if p, opts, err := g.puller(out, g.logger()); err == nil {
			p.Docker = cli
			if err := p.Pull(ctx, "docker/model-runner:latest", opts); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "ddm: could not pre-fetch the runner image (%v); docker will pull it.\n", err)
			}
		}
	}
	if tail, err := cli.Stream(ctx, os.Stderr, enable...); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("`docker %s` failed: %s", strings.Join(enable, " "), lastLine(tail))
	}
	// The runner takes a moment to come up.
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := modelrunner.Detect(ctx, cli)
		if err == nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// confirm asks a yes/no question on the terminal; without one it answers no.
func confirm(question string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
