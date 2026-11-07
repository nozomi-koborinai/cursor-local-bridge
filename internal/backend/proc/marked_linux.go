package proc

import (
	"os"
	"strconv"
	"syscall"
)

// marked returns the pids of this user's processes, other than this one,
// whose environment holds entry. It skips processes it cannot read.
func marked(entry string) []int {
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
		if syscall.Stat("/proc/"+name, &st) != nil || st.Uid != uid {
			continue
		}
		env, err := os.ReadFile("/proc/" + name + "/environ")
		if err == nil && hasEntry(env, entry) {
			pids = append(pids, pid)
		}
	}
	return pids
}
