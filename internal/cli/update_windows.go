package cli

import (
	"fmt"
	"os"
)

// swapBinary replaces exe. Windows won't overwrite a running executable, but
// it does let you rename it, so the old file is moved aside first.
func swapBinary(exe, next string) error {
	old := exe + ".old"
	_ = os.Remove(old) // left over from an earlier update
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("moving the old ddm aside: %w", err)
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe)
		return fmt.Errorf("installing the new ddm: %w", err)
	}
	_ = os.Remove(old) // fails while this process runs; removed on the next update
	return nil
}
