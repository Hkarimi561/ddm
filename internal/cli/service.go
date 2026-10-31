package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Hkarimi561/ddm/internal/uiauth"
)

// errNoServiceManager means the OS has no per-user service manager ddm can
// use (e.g. Linux without systemd); the UI is then started in the
// background for this session only.
var errNoServiceManager = errors.New("no service manager available")

func newServiceCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Keep the web UI running in the background, starting at login",
		Long: `The web UI lets you change settings (bandwidth limit, connections, proxy,
mirrors, ...), watch, pause and throttle downloads, and clean up the cache
from your browser. "ddm service install" (run by the installers) starts it
now and at every login:

  Linux    a systemd user service (ddm.service)
  macOS    a LaunchAgent
  Windows  a hidden autostart entry for your user

It asks for a password, which install generates if none is set yet.`,
	}
	cmd.AddCommand(newServiceInstallCmd(g), newServiceUninstallCmd(g), newServiceStatusCmd(g), newServicePasswordCmd(g))
	return cmd
}

func (g *globals) uiAddressAndPassword() (address, pwPath string, err error) {
	cfg, err := g.config()
	if err != nil {
		return "", "", err
	}
	return cfg.Config.UIAddress, uiauth.PasswordPath(cfg.Path), nil
}

func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func newServiceInstallCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Start the web UI now and at every login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			address, pwPath, err := g.uiAddressAndPassword()
			if err != nil {
				return err
			}
			generated := ""
			if !uiauth.Exists(pwPath) {
				generated = uiauth.Generate()
				if err := uiauth.Set(pwPath, generated); err != nil {
					return err
				}
			}
			exe, err := executable()
			if err != nil {
				return err
			}
			g.stopUI() // an older instance (e.g. before an upgrade)
			err = installService(exe)
			if errors.Is(err, errNoServiceManager) {
				fmt.Println("No service manager found; starting the web UI for this session only.")
				err = startDetached(exe, g.uiLogPath())
			}
			if err != nil {
				return fmt.Errorf("setting up the web UI service: %w\nYou can run `ddm serve` yourself instead.", err)
			}
			url := "http://" + displayAddr(address)
			if waitHealthy(cmd.Context(), address, 15*time.Second) {
				fmt.Printf("Web UI running: %s\n", url)
			} else {
				fmt.Printf("Web UI installed: %s (it may take a moment to start; check with `ddm service status`)\n", url)
			}
			if generated != "" {
				fmt.Printf("Password: %s\n(Change it in the web UI or with `ddm service password`.)\n", generated)
			} else {
				fmt.Println("Log in with your existing password (reset it with `ddm service password`).")
			}
			return nil
		},
	}
}

func newServiceUninstallCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the web UI and don't start it at login anymore",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := uninstallService(); err != nil && !errors.Is(err, errNoServiceManager) {
				return err
			}
			g.stopUI()
			fmt.Println("The web UI is stopped and won't start at login. Your settings and password are kept.")
			return nil
		},
	}
}

func newServiceStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the web UI is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			address, pwPath, err := g.uiAddressAndPassword()
			if err != nil {
				return err
			}
			installed := "not installed (run `ddm service install`)"
			if serviceInstalled() {
				installed = "installed, starts at login"
			}
			running := "not running"
			if waitHealthy(cmd.Context(), address, 2*time.Second) {
				running = "running at http://" + displayAddr(address)
			}
			password := "set"
			if !uiauth.Exists(pwPath) {
				password = "not set (one is generated on first start)"
			}
			fmt.Printf("Service:  %s\nWeb UI:   %s\nPassword: %s\n", installed, running, password)
			return nil
		},
	}
}

func newServicePasswordCmd(g *globals) *cobra.Command {
	var generate bool
	cmd := &cobra.Command{
		Use:   "password",
		Short: "Set a new web UI password (logs out every browser)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, pwPath, err := g.uiAddressAndPassword()
			if err != nil {
				return err
			}
			var pw string
			if generate || !term.IsTerminal(int(os.Stdin.Fd())) {
				pw = uiauth.Generate()
			} else {
				fmt.Fprint(os.Stderr, "New password: ")
				a, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
				if err != nil {
					return err
				}
				fmt.Fprint(os.Stderr, "Repeat it: ")
				b, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
				if err != nil {
					return err
				}
				if string(a) != string(b) {
					return errors.New("the passwords don't match")
				}
				pw = string(a)
			}
			if err := uiauth.Set(pwPath, pw); err != nil {
				return err
			}
			if generate || !term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Printf("New web UI password: %s\n", pw)
			} else {
				fmt.Println("Password changed. Browsers that were logged in have to log in again.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&generate, "generate", false, "generate a random password and print it")
	return cmd
}

// waitHealthy polls the UI's health endpoint until it answers or timeout.
func waitHealthy(ctx context.Context, address string, timeout time.Duration) bool {
	url := "http://" + displayAddr(address) + "/api/health"
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (g *globals) uiPIDPath() string {
	c, err := g.cache()
	if err != nil {
		return ""
	}
	return filepath.Join(c.Root, "run", "ui.pid")
}

func (g *globals) uiLogPath() string {
	c, err := g.cache()
	if err != nil {
		return os.DevNull
	}
	return filepath.Join(c.Root, "ui.log")
}

// stopUI stops a running UI server found through its PID file.
func (g *globals) stopUI() {
	path := g.uiPIDPath()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid == os.Getpid() {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
	_ = os.Remove(path)
	time.Sleep(300 * time.Millisecond) // let the port free up
}
