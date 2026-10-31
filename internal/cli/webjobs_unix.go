//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func cmdEnv() []string { return os.Environ() }

// prepareChild puts the command in its own process group, so cancelling it
// also stops the docker process it started.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
