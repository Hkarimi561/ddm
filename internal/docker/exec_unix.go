//go:build !windows

package docker

import (
	"fmt"
	"os"
	"syscall"
)

// Exec replaces ddm with `docker args...`. Docker inherits the terminal,
// standard streams, signals and process ID, so the experience (including the
// exit code) is exactly that of running docker directly. It returns only if
// the exec itself fails.
func Exec(bin string, args []string) (int, error) {
	err := syscall.Exec(bin, append([]string{"docker"}, args...), os.Environ())
	return 1, fmt.Errorf("running %s: %w", bin, err)
}
