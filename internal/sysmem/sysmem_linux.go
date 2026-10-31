package sysmem

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func total() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kib << 10, nil
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
