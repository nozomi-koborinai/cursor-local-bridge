package proc

import (
	"bytes"
	"encoding/binary"
	"os"

	"golang.org/x/sys/unix"
)

// marked returns the pids of this user's processes, other than this one,
// whose environment holds entry. It skips processes it cannot read.
func marked(entry string) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, kp := range procs {
		pid := int(kp.Proc.P_pid)
		if pid == self {
			continue
		}
		args, err := unix.SysctlRaw("kern.procargs2", pid)
		if err == nil && hasEntry(environ(args), entry) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// environ returns the environment strings in a kern.procargs2 buffer, which
// holds argc as a 32-bit int, the executable path, NUL padding, argc
// arguments, and then the environment.
func environ(args []byte) []byte {
	if len(args) < 4 {
		return nil
	}
	argc := binary.NativeEndian.Uint32(args)
	_, rest, ok := bytes.Cut(args[4:], []byte{0})
	if !ok {
		return nil
	}
	rest = bytes.TrimLeft(rest, "\x00")
	for range argc {
		if _, rest, ok = bytes.Cut(rest, []byte{0}); !ok {
			return nil
		}
	}
	return rest
}
