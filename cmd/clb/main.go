// Command clb runs local Cursor CLI agents with a cloud-agent-shaped
// lifecycle.
package main

import (
	"os"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
