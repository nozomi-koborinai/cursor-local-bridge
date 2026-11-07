package proc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// userPids returns the pids of this user's processes other than this one.
func userPids() []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil
	}
	self := os.Getpid()
	pids := make([]int, 0, len(procs))
	for _, kp := range procs {
		if pid := int(kp.Proc.P_pid); pid != self {
			pids = append(pids, pid)
		}
	}
	return pids
}

func environOf(pid int) ([]byte, error) {
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return environ(args), nil
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

// The PROC_PIDVNODEPATHINFO flavor of proc_pidinfo fills a struct
// proc_vnodepathinfo from <sys/proc_info.h>: the working directory and then
// the root directory, each a 152-byte vnode_info followed by a NUL-terminated
// path of up to 1024 bytes.
const (
	procInfoCallPidInfo  = 2
	procPidVnodePathInfo = 9
	vnodeInfoSize        = 152
	maxPathLen           = 1024
)

// cwdOf calls proc_info, the system call behind libproc's proc_pidinfo.
func cwdOf(pid int) (string, error) {
	var info [2 * (vnodeInfoSize + maxPathLen)]byte
	//nolint:staticcheck // x/sys/unix wraps no libSystem call for proc_info, and this package does not use cgo.
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidInfo, uintptr(pid), procPidVnodePathInfo, 0,
		uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 {
		return "", errno
	}
	if n != uintptr(len(info)) {
		return "", fmt.Errorf("proc_info filled %d bytes, want %d", n, len(info))
	}
	path, _, _ := bytes.Cut(info[vnodeInfoSize:vnodeInfoSize+maxPathLen], []byte{0})
	return string(path), nil
}
