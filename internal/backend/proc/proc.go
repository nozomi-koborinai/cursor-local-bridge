// Package proc runs an agent CLI process in its own process group, so that
// stopping it also stops the processes it spawned, and keeps the beginning of
// its stderr for error messages.
package proc

import (
	"fmt"
	"os"
	"os/exec"
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
	stderr   head
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
