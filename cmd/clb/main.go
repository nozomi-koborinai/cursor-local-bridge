// Command clb runs local Cursor CLI agents with a cloud-agent-shaped
// lifecycle.
package main

import (
	"cmp"
	"os"
	"runtime/debug"
	"strings"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/cli"
)

// version is set by GoReleaser with -X main.version.
var version string

func main() {
	os.Exit(cli.Execute(cmp.Or(version, moduleVersion())))
}

func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}
