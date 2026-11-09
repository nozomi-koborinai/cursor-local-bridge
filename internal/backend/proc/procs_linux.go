package proc

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// userPids returns the pids of this user's processes other than this one.
func userPids() []int {
	dir, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	names, _ := dir.Readdirnames(-1)
	_ = dir.Close()
	uid, self := uint32(os.Geteuid()), os.Getpid()
	var pids []int
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid == self {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat("/proc/"+name, &st) == nil && st.Uid == uid {
			pids = append(pids, pid)
		}
	}
	return pids
}

func environOf(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
}

func cwdOf(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
}

func processIdentity(pid int) (uint64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) <= 19 {
		return 0, errors.New("short stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
