package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func systemdUnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", "ddm.service"), nil
}

func systemdUnit(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=ddm web UI (settings and downloads)
After=network-online.target

[Service]
ExecStart=%s serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, quoteSystemd(exe))
}

func quoteSystemd(s string) string {
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// installService installs a systemd user service. It falls back to
// errNoServiceManager where systemd isn't running for the user (WSL without
// systemd, containers).
func installService(exe string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errNoServiceManager
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		return errNoServiceManager
	}
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(systemdUnit(exe)), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "ddm.service"); err != nil {
		return err
	}
	if err := systemctl("restart", "ddm.service"); err != nil {
		return err
	}
	fmt.Println("Installed the systemd user service ddm.service.")
	fmt.Println("To keep it running while you are logged out: loginctl enable-linger $USER")
	return nil
}

func uninstallService() error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return errNoServiceManager
	}
	_ = systemctl("disable", "--now", "ddm.service")
	if err := os.Remove(path); err != nil {
		return err
	}
	return systemctl("daemon-reload")
}

func serviceInstalled() bool {
	path, err := systemdUnitPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
