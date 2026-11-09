//go:build darwin

package keepawake

import (
	"context"
	"os"
)

func defaultHold() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return runCaffeinate(ctx, "/usr/bin/caffeinate", os.Getpid())
	}
}
