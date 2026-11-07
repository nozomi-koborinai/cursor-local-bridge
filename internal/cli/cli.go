// Package cli is the clb command line.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Execute runs the command line and returns the process exit code.
func Execute(version string) int {
	root := &cobra.Command{
		Use:           "clb",
		Short:         "Run local Cursor CLI agents with a cloud-agent-shaped lifecycle",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clb:", err)
		return 1
	}
	return 0
}
