//go:build windows

package docker

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// Exec runs `docker args...` with ddm's console and standard streams and
// returns its exit code. Windows can't replace a running process, so ddm
// waits for docker instead. Ctrl+C reaches docker directly through the
// shared console; ddm ignores it, so docker decides what it means (e.g.
// detaching from a container) instead of ddm dying first.
func Exec(bin string, args []string) (int, error) {
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
