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

// group returns the pids of the processes in process group pgid.
func group(pgid int) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil
	}
	pids := make([]int, len(procs))
	for i, kp := range procs {
		pids[i] = int(kp.Proc.P_pid)
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
// path of up to 1024 bytes. The PROC_PIDUNIQIDENTIFIERINFO flavor fills a
// 56-byte struct proc_uniqidentifierinfo, which holds the process's unique ID
// at offset 16 and its parent's at offset 24.
const (
	procInfoCallListPids      = 1
	procInfoCallPidInfo       = 2
	procPPIDOnly              = 6
	procPidVnodePathInfo      = 9
	procPidUniqIdentifierInfo = 17
	vnodeInfoSize             = 152
	maxPathLen                = 1024
	uniqIdentifierInfoSize    = 56
)

func cwdOf(pid int) (string, error) {
	var info [2 * (vnodeInfoSize + maxPathLen)]byte
	if err := pidInfo(pid, procPidVnodePathInfo, info[:]); err != nil {
		return "", err
	}
	path, _, _ := bytes.Cut(info[vnodeInfoSize:vnodeInfoSize+maxPathLen], []byte{0})
	return string(path), nil
}

// identity returns the unique IDs of pid and of its parent, which the kernel
// never reuses. Once the parent exits, the parent ID it reports is launchd's.
func identity(pid int) (id, parent uint64, err error) {
	var info [uniqIdentifierInfoSize]byte
	if err := pidInfo(pid, procPidUniqIdentifierInfo, info[:]); err != nil {
		return 0, 0, err
	}
	return binary.NativeEndian.Uint64(info[16:]), binary.NativeEndian.Uint64(info[24:]), nil
}

func processIdentity(pid int) (uint64, error) {
	id, _, err := identity(pid)
	return id, err
}

// pidInfo calls proc_info, the system call behind libproc's proc_pidinfo.
func pidInfo(pid, flavor int, info []byte) error {
	//nolint:staticcheck // x/sys/unix wraps no libSystem call for proc_info, and this package does not use cgo.
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidInfo, uintptr(pid), uintptr(flavor), 0,
		uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 {
		return errno
	}
	if n != uintptr(len(info)) {
		return fmt.Errorf("proc_info filled %d bytes, want %d", n, len(info))
	}
	return nil
}

// children returns the pids of the processes whose parent is pid. It calls
// proc_info the way libproc's proc_listpids does, which fills the buffer
// with pids and returns the number of bytes it filled.
func children(pid int) ([]int, error) {
	buf := make([]int32, 16)
	for {
		//nolint:staticcheck // x/sys/unix wraps no libSystem call for proc_info, and this package does not use cgo.
		n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallListPids, procPPIDOnly, uintptr(pid), 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(4*len(buf)))
		if errno != 0 {
			return nil, errno
		}
		if count := int(n) / 4; count < len(buf) {
			pids := make([]int, count)
			for i, child := range buf[:count] {
				pids[i] = int(child)
			}
			return pids, nil
		}
		buf = make([]int32, 2*len(buf))
	}
}
