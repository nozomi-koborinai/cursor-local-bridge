//go:build !darwin

package keepawake

import "context"

func defaultHold() func(ctx context.Context) error {
	return nil
}
