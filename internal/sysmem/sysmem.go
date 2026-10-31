// Package sysmem reports how much memory the machine has, to warn before
// loading a model that won't fit.
package sysmem

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Total returns the machine's physical memory in bytes.
func Total() (uint64, error) { return total() }

// GPUMemory returns the memory of the largest NVIDIA GPU in bytes, or 0 if
// there is none (or nvidia-smi isn't installed).
func GPUMemory() uint64 {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	var best uint64
	for _, line := range strings.Split(string(out), "\n") {
		mib, err := strconv.ParseUint(strings.TrimSpace(line), 10, 64)
		if err == nil && mib<<20 > best {
			best = mib << 20
		}
	}
	return best
}
