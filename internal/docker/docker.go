// Package docker drives the local Docker CLI: checking that the daemon is
// reachable, inspecting images, loading downloaded images, and handing
// control to docker for pass-through commands.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
)

var (
	// ErrNotInstalled means no docker CLI was found in PATH.
	ErrNotInstalled = errors.New("the docker command was not found in PATH")
	// ErrDaemonUnavailable means the CLI could not reach a Docker engine.
	ErrDaemonUnavailable = errors.New("the Docker engine is not running or not reachable")
)

// CLI runs docker commands. Going through the CLI (rather than the Engine
// API directly) means DOCKER_HOST, docker contexts and Docker Desktop's
// sockets all behave exactly as they do for the user.
type CLI struct {
	Bin string
	// Global options (e.g. --context remote) put before every command, so
	// ddm talks to the same engine as the command it is helping.
	Global []string
}

// Find locates the docker CLI.
func Find() (*CLI, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("%w.\n%s", ErrNotInstalled, InstallHint())
	}
	return &CLI{Bin: bin}, nil
}

// InstallHint tells the user how to install Docker on this OS.
func InstallHint() string {
	switch runtime.GOOS {
	case "windows":
		return "Install Docker Desktop: https://docs.docker.com/desktop/setup/install/windows-install/"
	case "darwin":
		return "Install Docker Desktop: https://docs.docker.com/desktop/setup/install/mac-install/"
	default:
		return "Install Docker Engine: https://docs.docker.com/engine/install/ (or Docker Desktop for Linux)"
	}
}

// StartHint tells the user how to get the engine running, based on the
// error the CLI reported.
func StartHint(errText string) string {
	low := strings.ToLower(errText)
	if runtime.GOOS == "linux" && strings.Contains(low, "permission denied") {
		return "Your user may not access the Docker socket. Run `sudo usermod -aG docker $USER`, then log out and back in."
	}
	switch runtime.GOOS {
	case "windows", "darwin":
		return "Start Docker Desktop and wait until it reports that the engine is running, then try again."
	default:
		return "Start the engine with `sudo systemctl start docker` (or start Docker Desktop), then try again."
	}
}

// ServerPlatform returns the engine's platform, e.g. "linux/amd64". It is
// also the cheapest way to check that the engine is reachable.
func (c *CLI) ServerPlatform(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil || out == "/" || out == "" {
		msg := "no server information"
		if err != nil {
			msg = err.Error()
		}
		return "", fmt.Errorf("%w (%s)\n%s", ErrDaemonUnavailable, msg, StartHint(msg))
	}
	return out, nil
}

// ImageID returns the ID of the local image ref, or "" if there is none.
func (c *CLI) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// Tag tags image src as dst.
func (c *CLI) Tag(ctx context.Context, src, dst string) error {
	_, err := c.run(ctx, "tag", src, dst)
	return err
}

// Stream runs a docker command with its output going to w, and returns
// the tail of its error output for diagnosis when it fails.
func (c *CLI) Stream(ctx context.Context, w io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, c.args(args...)...)
	var errBuf bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = io.MultiWriter(w, &errBuf)
	err := cmd.Run()
	tail := strings.TrimSpace(errBuf.String())
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	return tail, err
}

// Output runs a docker command and returns its trimmed standard output.
func (c *CLI) Output(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, args...)
}

// Load streams the image archive produced by write into `docker load` and
// returns docker's output. Docker only registers the image once the whole
// archive has been read and checked, so a failure part-way through (a
// broken pipe, a corrupt blob, Ctrl+C) leaves no half-imported image behind.
func (c *CLI) Load(ctx context.Context, write func(io.Writer) error) (string, error) {
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, c.Bin, c.args("load")...)
	cmd.Stdin = pr
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	werr := make(chan error, 1)
	go func() {
		err := write(pw)
		pw.CloseWithError(err)
		werr <- err
	}()
	err := cmd.Wait()
	_ = pr.Close() // unblock the writer if docker exited early
	writeErr := <-werr

	// A writer failure (other than docker hanging up on us) is the root cause.
	if writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
		return "", writeErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("docker load failed: %s", strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (c *CLI) args(args ...string) []string {
	return append(append([]string(nil), c.Global...), args...)
}

// raw runs a docker command and returns stdout even when it fails, which
// `docker info` and `docker version` need: they print what they know
// before reporting that the engine is unreachable.
func (c *CLI) raw(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, c.Bin, c.args(args...)...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return strings.TrimSpace(o.String()), strings.TrimSpace(e.String()), err
}

func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	out, msg, err := c.raw(ctx, args...)
	if err != nil {
		if msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return out, nil
}
