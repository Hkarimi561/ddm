//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts `ddm serve` in its own session, so it keeps running
// after the terminal closes, logging to logPath.
func startDetached(exe, logPath string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
