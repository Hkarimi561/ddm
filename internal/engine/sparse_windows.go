//go:build windows

package engine

import (
	"os"

	"golang.org/x/sys/windows"
)

const fsctlSetSparse = 0x000900c4

// makeSparse marks f as sparse. Without it, writing a segment at a high
// offset of a freshly sized file makes NTFS zero-fill everything before it
// first, which stalls multi-gigabyte downloads.
func makeSparse(f *os.File) error {
	var n uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &n, nil)
}
