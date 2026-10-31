//go:build !windows

package cli

import "os"

// swapBinary atomically replaces exe; a running ddm keeps its old inode.
func swapBinary(exe, next string) error {
	return os.Rename(next, exe)
}
