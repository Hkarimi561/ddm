package cli

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func cmdEnv() []string { return os.Environ() }

// prepareChild keeps a console window from opening for the command.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// killTree stops the command and the docker process it started.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := k.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
