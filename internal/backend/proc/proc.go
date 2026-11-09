// Package proc runs an agent CLI process in its own process group, so that
// stopping it also stops the processes it spawned, and keeps the beginning of
// its stderr for error messages. Stopping it also ends the descendants that
// leave the group. A token in the process's environment marks them where
// environments can be read. macOS can hide environments even from their own
// user, so there the package also watches the process and each descendant for
// forks.
// EndUnder ends the processes working in a directory, which also finds the
// descendants that cleared their environment.
package proc

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

	// StderrLineLimit is the longest piece of a stderr line that Start passes
	// on. A longer line arrives in pieces cut at rune boundaries.
	StderrLineLimit = 4 << 10
	// StderrLogLimit is how many bytes of one process's stderr Start passes
	// on.
	StderrLogLimit = 1 << 20
)

// StderrLine is one line, or one piece of a long line, that a process wrote
// to stderr, without its line ending.
type StderrLine struct {
	Text string
	// Capped marks the one call Start makes when the process has written more
	// than StderrLogLimit bytes. Text is empty, and no calls follow.
	Capped bool
}

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
	family   *family
	stderr   *stderrLog
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

// Start starts cmd in a new process group and adds CLB_PROC_TOKEN, with a
// value unique to this process, to its environment. The caller may set
// cmd.Stdin and cmd.Env but not cmd.Stdout or cmd.Stderr.
//
// When onStderr is not nil, Start passes it what the process writes to
// stderr, line by line, in order, from one goroutine at a time. A final line
// with no line ending is passed after the process exits and before Done
// closes. Wait does not return while onStderr runs, so onStderr must not
// block on anything that waits for the process.
func Start(cmd *exec.Cmd, onStderr func(StderrLine)) (*Process, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &Process{
		Stdout: r,
		cmd:    cmd,
		mark:   tokenVar + "=" + hex.EncodeToString(token[:]),
		stderr: &stderrLog{onLine: onStderr},
		done:   make(chan struct{}),
	}
	cmd.Env = append(cmd.Environ(), p.mark)
	cmd.Stdout = w
	cmd.Stderr = p.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = StderrWait
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	// Before Wait can reap the process and free its pid for reuse.
	p.family = watchFamily(cmd.Process.Pid)
	go func() {
		p.err = cmd.Wait()
		p.stderr.flush()
		close(p.done)
	}()
	return p, nil
}

// PID is the process ID, which is also its process group ID.
func (p *Process) PID() int { return p.cmd.Process.Pid }

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
// their environment, so on Linux it misses a descendant that dropped the
// token. macOS can hide environments, so there it also ends the descendants
// it saw being born, watching the process and each descendant for forks from
// Start on, and the processes left in a group one of them led. There it
// misses a descendant only when the token is not visible, the descendant
// left its parent's group, and the parent exited before the watch saw the
// fork. It returns after the process has exited and is safe to call more
// than once.
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
			if p.family.owns(pid) {
				return true
			}
			env, err := environOf(pid)
			return err == nil && hasEntry(env, p.mark)
		})
		p.family.close()
	})
}

// EndUnder ends this user's processes, other than this one, whose working
// directory is dir or below it, the way Stop ends the descendants that left
// the group. It does nothing when dir is relative, names a symlink, or
// resolves to the root. A directory that has already been removed is still
// swept under the path the kernel reports for it.
func EndUnder(dir string) {
	root, ok := sweepRoot(dir)
	if !ok {
		return
	}
	reap(func(pid int) bool {
		cwd, err := cwdOf(pid)
		return err == nil && cwdUnder(cwd, root)
	})
}

// sweepRoot is the directory EndUnder matches. Symlinks in the parent are
// resolved, because kernels report those paths resolved, and the leaf name is
// joined back on, so a removed directory still matches. A symlink at dir
// itself is refused: matching that target would end processes outside the
// worktree when the leaf is replaced by one.
func sweepRoot(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return "", false
	}
	info, err := os.Lstat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return "", false
	}
	root := filepath.Join(parent, filepath.Base(dir))
	if root == "/" {
		return "", false
	}
	return root, true
}

// Linux reports a removed working directory with a " (deleted)" suffix.
func cwdUnder(cwd, root string) bool {
	cwd = strings.TrimSuffix(cwd, " (deleted)")
	return cwd == root || strings.HasPrefix(cwd, root+"/")
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

// stderrLog is cmd.Stderr. It keeps the first stderrSize bytes for Explain
// and cuts everything into lines for onLine. Its only writer is exec.Cmd's
// copying goroutine. Start calls flush after cmd.Wait returns, when that
// goroutine has finished (Wait awaits it even after WaitDelay), so only head
// and dropped need mu, for Explain.
type stderrLog struct {
	onLine func(StderrLine)

	mu      sync.Mutex
	head    []byte
	dropped int

	partial []byte
	passed  int
	capped  bool
}

func (s *stderrLog) Write(b []byte) (int, error) {
	s.keep(b)
	if s.onLine == nil || s.capped {
		return len(b), nil
	}
	s.partial = append(s.partial, b...)
	s.drain()
	if s.capped || len(s.partial) == 0 {
		s.partial = nil
		return len(b), nil
	}
	rest := make([]byte, len(s.partial))
	copy(rest, s.partial)
	s.partial = rest
	return len(b), nil
}

func (s *stderrLog) keep(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(len(b), stderrSize-len(s.head))
	s.head = append(s.head, b[:n]...)
	s.dropped += len(b) - n
}

func (s *stderrLog) drain() {
	for !s.capped {
		nl := bytes.IndexByte(s.partial, '\n')
		if nl >= 0 && nl <= StderrLineLimit {
			line := s.partial[:nl]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			raw := nl + 1
			s.partial = s.partial[raw:]
			s.emit(raw, line)
			continue
		}
		if len(s.partial) <= StderrLineLimit {
			return
		}
		cut := cutAtRune(s.partial, StderrLineLimit)
		piece := s.partial[:cut]
		s.partial = s.partial[cut:]
		s.emit(cut, piece)
	}
}

func (s *stderrLog) emit(raw int, text []byte) {
	if s.capped {
		return
	}
	if s.passed+raw > StderrLogLimit {
		s.capped = true
		s.partial = nil
		s.onLine(StderrLine{Capped: true})
		return
	}
	s.passed += raw
	s.onLine(StderrLine{Text: string(text)})
}

func (s *stderrLog) flush() {
	if s.onLine == nil || s.capped || len(s.partial) == 0 {
		return
	}
	s.drain()
	if s.capped || len(s.partial) == 0 {
		s.partial = nil
		return
	}
	text := s.partial
	s.partial = nil
	s.emit(len(text), text)
}

func (s *stderrLog) contents() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.head), s.dropped
}

func cutAtRune(b []byte, limit int) int {
	if limit > len(b) {
		limit = len(b)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	if cut > 0 {
		return cut
	}
	_, size := utf8.DecodeRune(b)
	if size < 1 {
		return 1
	}
	return size
}
