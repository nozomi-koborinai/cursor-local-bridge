package proc

import (
	"bufio"
	"errors"
	"io"
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

func TestStderrKeepsOnlyTheTail(t *testing.T) {
	p, err := Start(exec.Command("sh", "-c", `head -c 20000 /dev/zero | tr '\0' a >&2; echo END >&2`))
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	_ = p.Stdout.Close()
	msg := p.Explain(errors.New("failed")).Error()
	if !strings.HasSuffix(msg, "END") || len(msg) > tailSize+len("failed; stderr: ") {
		t.Errorf("Explain kept %d bytes ending %q", len(msg), msg[len(msg)-10:])
	}
}
