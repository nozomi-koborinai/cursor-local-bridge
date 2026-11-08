//go:build e2e && darwin

package e2e

import "golang.org/x/sys/unix"

func echoEnabled(fd int) (bool, error) {
	state, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return false, err
	}
	return state.Lflag&unix.ECHO != 0, nil
}
