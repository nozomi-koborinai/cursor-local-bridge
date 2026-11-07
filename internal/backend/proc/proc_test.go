package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("PROC_TEST_DETACH") == "1" {
		detach()
	}
	os.Exit(m.Run())
}

// detach starts a sleep in a new session, prints its pid, and exits, which
// leaves the sleep outside the process group that Stop kills.
func detach() {
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(cmd.Process.Pid)
	os.Exit(0)
}

func TestStopEndsDescendantsThatLeftTheGroup(t *testing.T) {
	dir := t.TempDir()
	p, grandchild := startDetaching(t, dir)
	_, otherGrandchild := startDetaching(t, dir)

	bystander := exec.Command("sleep", "120")
	bystander.Dir = dir
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		_ = bystander.Wait()
		close(ended)
	}()
	t.Cleanup(func() {
		_ = bystander.Process.Kill()
		<-ended
	})

	p.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(grandchild, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(grandchild, syscall.SIGKILL)
			t.Fatalf("process %d, which the stopped process started in a new session, still runs 5s after Stop", grandchild)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-ended:
		t.Error("Stop ended a sleep that the test started itself in the same directory")
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Kill(otherGrandchild, 0); err != nil {
		t.Errorf("Stop ended process %d, which another Process left running: kill(0) = %v", otherGrandchild, err)
	}
}

// startDetaching returns the Process of an exited detach helper and the pid
// of the sleep it left running.
func startDetaching(t *testing.T, dir string) (*Process, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A helper that misses PROC_TEST_DETACH runs no tests instead of this
	// one, which would start helpers of its own.
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PROC_TEST_DETACH=1")
	p, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	out, err := io.ReadAll(p.Stdout)
	_ = p.Stdout.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("detach helper: %v", p.Explain(err))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	return p, pid
}

func TestStopEndsChildrenThatIgnoreSIGTERM(t *testing.T) {
	p, err := Start(exec.Command("sh", "-c", `(trap "" TERM; exec sleep 60) & echo $!; wait`))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
	p.Stop()
	_ = p.Stdout.Close()

	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running after Stop", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOutputBeforeExitIsKept(t *testing.T) {
	p, err := Start(exec.Command("sh", "-c", `echo last words; echo oops >&2; exit 3`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(p.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "last words\n" {
		t.Errorf("stdout = %q", out)
	}
	werr := p.Wait()
	var exitErr *exec.ExitError
	if !errors.As(werr, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("Wait = %v, want exit status 3", werr)
	}
	if msg := p.Explain(werr).Error(); !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "oops") {
		t.Errorf("Explain = %q, want exit status and stderr", msg)
	}
}

func TestExplainShowsTheStartOfStderr(t *testing.T) {
	a512 := strings.Repeat("a", 512)
	tests := []struct {
		name, stderr, want string
	}{
		{"512 bytes are not cut", "\n" + a512 + "\n", "failed; stderr: " + a512},
		{
			"longer is cut before a split rune",
			"\nError: " + strings.Repeat("é", 600) + "\n",
			"failed; stderr: Error: " + strings.Repeat("é", 252) + " [696 more bytes]",
		},
		{
			"longer than the capture keeps its first line",
			"Error: first line\n" + strings.Repeat("a", 20000) + "END",
			"failed; stderr: Error: first line\n" + strings.Repeat("a", 512-18) + " [19509 more bytes]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", `printf '%s' "$STDERR" >&2`)
			cmd.Env = append(os.Environ(), "STDERR="+tt.stderr)
			p, err := Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			_ = p.Wait()
			_ = p.Stdout.Close()
			if got := p.Explain(errors.New("failed")).Error(); got != tt.want {
				t.Errorf("Explain = %s, want %s", brief(got), brief(tt.want))
			}
		})
	}
}

func brief(s string) string {
	if len(s) <= 100 {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%q...%q (%d bytes)", s[:50], s[len(s)-50:], len(s))
}
