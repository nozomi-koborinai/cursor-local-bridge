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
