// Command ddm is a resumable download manager for Docker images.
package main

import (
	"os"

	"github.com/Hkarimi561/ddm/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Execute())
}
