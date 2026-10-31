// Package ui renders download progress: live multi-line bars on a terminal,
// periodic log lines otherwise.
package ui

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/Hkarimi561/ddm/internal/engine"
)

// Reporter is an engine.Progress that must be closed once downloads end.
type Reporter interface {
	engine.Progress
	Close()
}

// NewReporter returns live progress bars when f is a terminal and plain is
// false, and line-oriented output otherwise (CI logs, pipes, --verbose).
func NewReporter(f *os.File, total int64, plain bool) Reporter {
	if plain || !term.IsTerminal(int(f.Fd())) {
		return newLines(f, total)
	}
	return newBars(f, total)
}

// hostOf shortens a source name like "https://mirror.example.com/proxy" to
// "mirror.example.com/proxy" for display.
func hostOf(name string) string {
	if _, rest, ok := strings.Cut(name, "://"); ok {
		return rest
	}
	return name
}

// HumanBytes formats n using binary units, e.g. "1.5 GiB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
