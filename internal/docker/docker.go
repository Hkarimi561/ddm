// Package docker drives the local Docker CLI: checking that the daemon is
// reachable, inspecting images, and loading downloaded images.
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
	ErrDaemonUnavailable = errors.New("the Docker daemon is not running or not reachable")
)

// CLI runs docker commands. Going through the CLI (rather than the Engine
// API directly) means DOCKER_HOST, docker contexts and Docker Desktop's
// sockets all behave exactly as they do for the user.
type CLI struct {
	Bin string
}

// Find locates the docker CLI.
func Find() (*CLI, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("%w; install Docker from https://docs.docker.com/get-docker/", ErrNotInstalled)
	}
	return &CLI{Bin: bin}, nil
}

// ServerPlatform returns the engine's platform, e.g. "linux/amd64".
func (c *CLI) ServerPlatform(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil || out == "/" || out == "" {
		if err == nil {
			err = errors.New("no server information")
		}
		return "", fmt.Errorf("%w (%v)\n%s", ErrDaemonUnavailable, err, startHint())
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

// Load streams the image archive produced by write into `docker load` and
// returns docker's output. Docker only registers the image once the whole
// archive has been read and checked, so a failure part-way through (a
// broken pipe, a corrupt blob, Ctrl+C) leaves no half-imported image behind.
func (c *CLI) Load(ctx context.Context, write func(io.Writer) error) (string, error) {
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, c.Bin, "load")
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

func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func startHint() string {
	switch runtime.GOOS {
	case "windows", "darwin":
		return "Start Docker Desktop and wait until it reports that the engine is running, then try again."
	default:
		return "Start the engine with `sudo systemctl start docker` (or start Docker Desktop), then try again."
	}
}
