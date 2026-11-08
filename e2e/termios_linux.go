//go:build e2e && linux

package e2e

import "golang.org/x/sys/unix"

func echoEnabled(fd int) (bool, error) {
	state, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return false, err
	}
	return state.Lflag&unix.ECHO != 0, nil
}
