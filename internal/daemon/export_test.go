package daemon

import (
	"testing"
	"time"
)

// SetCloseWait sets closeWait to d until t ends. Call it before starting a
// daemon, so that t's cleanup restores it after the daemon has stopped.
func SetCloseWait(t *testing.T, d time.Duration) {
	old := closeWait
	closeWait = d
	t.Cleanup(func() { closeWait = old })
}
