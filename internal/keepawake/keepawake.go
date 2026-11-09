// Package keepawake holds a system sleep assertion on macOS while at least
// one run is active. On other platforms it does nothing.
package keepawake

import (
	"context"
	"os/exec"
	"strconv"
)

// Default returns a function that starts a sleep-preventing process tied to
// ctx when supported by the current OS, or nil when unsupported.
func Default() func(ctx context.Context) error {
	return defaultHold()
}

func runCaffeinate(ctx context.Context, bin string, pid int) error {
	cmd := exec.CommandContext(ctx, bin, "-i", "-w", strconv.Itoa(pid))
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}
