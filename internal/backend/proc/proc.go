// Package proc runs an agent CLI process in its own process group, so that
// stopping it also stops the processes it spawned, and keeps the beginning of
// its stderr for error messages. A token in the process's environment marks
// the descendants that leave the group, so that stopping it ends them too.
// EndUnder ends the processes working in a directory, which also finds the
// descendants that cleared their environment.
package proc

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	stderrSize  = 8 << 10
	explainSize = 512
	stopGrace   = 3 * time.Second
	reapGrace   = time.Second
	reapRounds  = 3
	reapPoll    = 50 * time.Millisecond
	tokenVar    = "CLB_PROC_TOKEN"
)

// StderrWait is how long Wait keeps reading stderr after the process exits,
// in case a child that outlives it still holds stderr open.
const StderrWait = time.Second

type Process struct {
	// Stdout is the read end of the command's stdout. The caller reads and
	// closes it. Unlike exec.Cmd.StdoutPipe, it stays readable after the
	// command exits, so output written just before an exit is not lost.
	Stdout *os.File

	cmd      *exec.Cmd
	mark     string
	stderr   head
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

// Start starts cmd in a new process group and adds CLB_PROC_TOKEN, with a
// value unique to this process, to its environment. The caller may set
// cmd.Stdin and cmd.Env but not cmd.Stdout or cmd.Stderr.
func Start(cmd *exec.Cmd) (*Process, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &Process{Stdout: r, cmd: cmd, mark: tokenVar + "=" + hex.EncodeToString(token[:]), done: make(chan struct{})}
	cmd.Env = append(cmd.Environ(), p.mark)
	cmd.Stdout = w
	cmd.Stderr = &p.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = StderrWait
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) Wait() error {
	<-p.done
	return p.err
}

// Explain appends the first explainSize bytes of the process's stderr to err,
// and how many bytes it left out. CLI errors lead with the message, and the
// result ends up in a run's error and its webhook payload.
func (p *Process) Explain(err error) error {
	text, dropped := p.stderr.contents()
	text = strings.TrimSpace(text)
	if text == "" {
		return err
	}
	cut := len(text)
	if cut > explainSize {
		cut = explainSize
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
	}
	if more := len(text) - cut + dropped; more > 0 {
		return fmt.Errorf("%w; stderr: %s [%d more bytes]", err, text[:cut], more)
	}
	return fmt.Errorf("%w; stderr: %s", err, text)
}

// Stop sends SIGTERM to the process group, waits up to 3s for the process to
// exit, then sends SIGKILL to the group so that children which ignored
// SIGTERM end too. Then it ends the descendants that left the group, such as
// a command the agent ran in its own session. It finds them by the token in
// their environment, so it misses a descendant that dropped the token. It
// returns after the process has exited and is safe to call more than once.
func (p *Process) Stop() {
	p.stopOnce.Do(func() {
		pgid := p.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(stopGrace):
		}
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
		reap(func(pid int) bool {
			env, err := environOf(pid)
			return err == nil && hasEntry(env, p.mark)
		})
	})
}

// EndUnder ends this user's processes, other than this one, whose working
// directory is dir or below it, the way Stop ends the descendants that left
// the group. It does nothing unless dir is absolute and not the root.
func EndUnder(dir string) {
	if !filepath.IsAbs(dir) {
		return
	}
	// Kernels report working directories with symlinks resolved.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		root = filepath.Clean(dir)
	}
	if root == "/" {
		return
	}
	reap(func(pid int) bool {
		cwd, err := cwdOf(pid)
		return err == nil && (cwd == root || strings.HasPrefix(cwd, root+"/"))
	})
}

// reap sends SIGTERM, then SIGKILL, to this user's processes, other than this
// one, that match. It scans again after each round of signals because a
// process can start another while it handles SIGTERM.
func reap(match func(pid int) bool) {
	scan := func() []int {
		var pids []int
		for _, pid := range userPids() {
			if match(pid) {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	sig := syscall.SIGTERM
	for range reapRounds {
		pids := scan()
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, sig)
		}
		sig = syscall.SIGKILL
		deadline := time.Now().Add(reapGrace)
		for len(pids) > 0 && time.Now().Before(deadline) {
			time.Sleep(reapPoll)
			pids = scan()
		}
	}
}

// hasEntry reports whether entry is one of the NUL-terminated strings in env
// before the first empty one.
func hasEntry(env []byte, entry string) bool {
	for e := range bytes.SplitSeq(env, []byte{0}) {
		if len(e) == 0 {
			return false
		}
		if string(e) == entry {
			return true
		}
	}
	return false
}

// head keeps the first stderrSize bytes written to it and counts the rest.
type head struct {
	mu      sync.Mutex
	buf     []byte
	dropped int
}

func (h *head) Write(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := min(len(b), stderrSize-len(h.buf))
	h.buf = append(h.buf, b[:n]...)
	h.dropped += len(b) - n
	return len(b), nil
}

func (h *head) contents() (string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.buf), h.dropped
}
