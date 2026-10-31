package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue   = "ddm-ui"
	vbsName    = "ddm-ui.vbs"
	appDataDir = "ddm"
)

// The UI is started through a tiny VBScript so that no console window
// opens at login.
func launcherPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), appDataDir, vbsName)
}

func launcherScript(exe string) string {
	q := strings.ReplaceAll(exe, `"`, `""`)
	return "' Starts the ddm web UI without a console window.\r\n" +
		"CreateObject(\"WScript.Shell\").Run \"\"\"" + q + "\"\" serve\", 0, False\r\n"
}

func installService(exe string) error {
	vbs := launcherPath()
	if err := os.MkdirAll(filepath.Dir(vbs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(vbs, []byte(launcherScript(exe)), 0o644); err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue(runValue, fmt.Sprintf(`wscript.exe "%s"`, vbs)); err != nil {
		return err
	}
	if err := exec.Command("wscript.exe", vbs).Start(); err != nil {
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
	_ = os.Remove(launcherPath())
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

// startDetached is only used where there is no service manager, which
// doesn't happen on Windows.
func startDetached(exe, logPath string) error {
	return exec.Command("wscript.exe", launcherPath()).Start()
}
