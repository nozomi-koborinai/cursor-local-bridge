package proc

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// familyWait bounds how long the watch runs on after close.
const familyWait = 100 * time.Millisecond

// A family is a process, the descendants seen being born to it, and the
// processes left in a group that a member led when it exited. Once a parent
// exits, the kernel reports launchd as its children's parent, so a child can
// only be seen being born while its parent runs. The family watches each
// member for forks to see it then, and for exits to catch the children it
// missed that stayed in the member's group.
type family struct {
	kq     int
	mu     sync.Mutex
	live   map[int]uint64 // pid to unique ID, for members that have not exited
	closed bool
}

// watchFamily starts watching pid and the descendants it starts. Call it
// before pid can be reaped. It returns nil if pid cannot be watched.
func watchFamily(pid int) *family {
	id, _, err := identity(pid)
	if err != nil {
		return nil
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return nil
	}
	f := &family{kq: kq, live: map[int]uint64{}}
	if !f.watch(pid, id) {
		_ = unix.Close(kq)
		return nil
	}
	f.discover(pid)
	go f.run()
	return f
}

// owns reports whether pid is a member that has not exited.
func (f *family) owns(pid int) bool {
	if f == nil {
		return false
	}
	want, ok := f.member(pid)
	if !ok {
		return false
	}
	id, _, err := identity(pid)
	return err == nil && id == want
}

// close ends the watch within familyWait without waiting for it.
func (f *family) close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *family) run() {
	defer func() { _ = unix.Close(f.kq) }()
	events := make([]unix.Kevent_t, 64)
	wait := unix.NsecToTimespec(int64(familyWait))
	for f.watching() {
		n, err := unix.Kevent(f.kq, nil, events, &wait)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return
		}
		for _, ev := range events[:n] {
			if ev.Fflags&unix.NOTE_FORK != 0 {
				f.discover(int(ev.Ident))
			}
		}
		for _, ev := range events[:n] {
			if ev.Fflags&unix.NOTE_EXIT != 0 {
				f.leave(int(ev.Ident))
			}
		}
	}
}

// watching is false once nothing is left to watch, because a process can
// join only through a live member.
func (f *family) watching() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.closed && len(f.live) > 0
}

// discover adds the children of member pid that are not members yet, then
// theirs. Each child is watched before its children are listed, so a child
// it forks later arrives as an event instead of being missed.
func (f *family) discover(pid int) {
	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		parent, ok := f.member(queue[0])
		if !ok {
			continue
		}
		kids, err := children(queue[0])
		if err != nil {
			continue
		}
		for _, kid := range kids {
			id, born, err := identity(kid)
			if err == nil && born == parent && f.watch(kid, id) {
				queue = append(queue, kid)
			}
		}
	}
}

// leave removes member pid, which has exited, and adds the processes still
// in the group it led, with their descendants. Only a process in pid's
// session can join that group, and the kernel does not hand out pid again
// while the group exists.
func (f *family) leave(pid int) {
	f.mu.Lock()
	delete(f.live, pid)
	f.mu.Unlock()
	for _, mate := range group(pid) {
		id, _, err := identity(mate)
		if err == nil && f.watch(mate, id) {
			f.discover(mate)
		}
	}
}

// watch adds pid as a member and reports whether it is a new one. It fails
// for a process that has already exited.
func (f *family) watch(pid int, id uint64) bool {
	if known, ok := f.member(pid); ok && known == id {
		return false
	}
	change := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
		Fflags: unix.NOTE_FORK | unix.NOTE_EXIT,
	}}
	if _, err := unix.Kevent(f.kq, change, nil, nil); err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[pid] = id
	return true
}

func (f *family) member(pid int) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.live[pid]
	return id, ok
}
