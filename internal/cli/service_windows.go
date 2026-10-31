package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue   = "ddm-ui"
	appDataDir = "ddm"
)

// Earlier versions started the UI through a VBScript, which antivirus
// software blocks. It is only removed now.
func legacyLauncherPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), appDataDir, "ddm-ui.vbs")
}

func installService(exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue(runValue, fmt.Sprintf(`"%s" service launch`, exe)); err != nil {
		return err
	}
	_ = os.Remove(legacyLauncherPath())
	if err := startDetached(exe, ""); err != nil {
		return err
	}
	fmt.Println("Registered the web UI to start when you log in to Windows.")
	return nil
}

func uninstallService() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	_ = os.Remove(legacyLauncherPath())
	return nil
}

func serviceInstalled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
)

// startDetached runs `ddm serve` with no console window, independent of
// the calling process. The web UI has its own log, so logPath is unused.
func startDetached(exe, _ string) error {
	cmd := exec.Command(exe, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | detachedProcess,
	}
	return cmd.Start()
}
