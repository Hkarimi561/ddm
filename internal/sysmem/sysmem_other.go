//go:build !linux && !darwin && !windows

package sysmem

import "errors"

func total() (uint64, error) { return 0, errors.New("unsupported OS") }
