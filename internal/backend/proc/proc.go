// Package proc runs an agent CLI process in its own process group, so that
// stopping it also stops the processes it spawned, and keeps the tail of its
// stderr for error messages.
package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tailSize  = 8 << 10
	stopGrace = 3 * time.Second
)

type Process struct {
	// Stdout is the read end of the command's stdout. The caller reads and
	// closes it. Unlike exec.Cmd.StdoutPipe, it stays readable after the
	// command exits, so output written just before an exit is not lost.
	Stdout *os.File

	cmd      *exec.Cmd
	stderr   tail
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

// Start starts cmd in a new process group. The caller may set cmd.Stdin but
// not cmd.Stdout or cmd.Stderr.
func Start(cmd *exec.Cmd) (*Process, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &Process{Stdout: r, cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = w
	cmd.Stderr = &p.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A child that outlives the process can hold stderr open; stop waiting
	// for it after this long.
	cmd.WaitDelay = time.Second
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

// Explain appends the tail of the process's stderr to err.
func (p *Process) Explain(err error) error {
	tail := strings.TrimSpace(p.stderr.String())
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w; stderr: %s", err, tail)
}

// Stop sends SIGTERM to the process group, waits up to 3s for the process to
// exit, then sends SIGKILL to the group so that children which ignored
// SIGTERM end too. It returns after the process has exited and is safe to
// call more than once.
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
	})
}

type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - tailSize; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
