package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if env, ok := os.LookupEnv("PROC_TEST_DETACH"); ok {
		detach(env)
	}
	os.Exit(m.Run())
}

// detach starts a sleep in a new session, prints its pid, and exits, which
// leaves the sleep outside the process group that Stop kills. With env
// "empty" the sleep gets an empty environment, the way the Cursor CLI runs
// terminal commands.
func detach(env string) {
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if env == "empty" {
		cmd.Env = []string{}
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(cmd.Process.Pid)
	os.Exit(0)
}

func TestStopEndsDescendantsThatLeftTheGroup(t *testing.T) {
	dir := t.TempDir()
	p, grandchild := startDetaching(t, dir, "inherit")
	_, otherGrandchild := startDetaching(t, dir, "inherit")

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

	waitGone(t, grandchild, "a sleep the stopped process started in a new session")
	select {
	case <-ended:
		t.Error("Stop ended a sleep that the test started itself in the same directory")
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Kill(otherGrandchild, 0); err != nil {
		t.Errorf("Stop ended process %d, which another Process left running: kill(0) = %v", otherGrandchild, err)
	}
}

func TestEndUnderEndsProcessesWorkingInTheDir(t *testing.T) {
	base := t.TempDir()
	run := mkdir(t, base, "run-1")
	_, inRun := startDetaching(t, run, "empty")
	_, below := startDetaching(t, mkdir(t, run, "src"), "empty")
	survivors := map[string]<-chan struct{}{
		"a sibling whose name extends the dir's": sleepIn(t, mkdir(t, base, "run-10")),
		"the parent":                             sleepIn(t, base),
	}

	EndUnder(run)

	waitGone(t, inRun, "a sleep working in the dir")
	waitGone(t, below, "a sleep working below the dir")
	time.Sleep(200 * time.Millisecond)
	for where, ended := range survivors {
		select {
		case <-ended:
			t.Errorf("EndUnder ended a sleep working in %s", where)
		default:
		}
	}
}

func TestEndUnderResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	run := mkdir(t, base, "run-1")
	link := filepath.Join(base, "link")
	if err := os.Symlink(run, link); err != nil {
		t.Fatal(err)
	}
	_, pid := startDetaching(t, run, "empty")

	EndUnder(link)

	waitGone(t, pid, "a sleep working in the dir the symlink names")
}

func TestEndUnderNeedsAnAbsoluteDirOtherThanTheRoot(t *testing.T) {
	base := t.TempDir()
	run := mkdir(t, base, "run-1")
	for name, target := range map[string]string{"link": run, "root": "/"} {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Fatal(err)
		}
	}
	inRun, inRoot := sleepIn(t, run), sleepIn(t, "/")
	t.Chdir(base)

	for _, dir := range []string{"run-1", "link", "/", filepath.Join(base, "root")} {
		EndUnder(dir)
	}

	time.Sleep(200 * time.Millisecond)
	select {
	case <-inRun:
		t.Error("EndUnder with a relative path ended a sleep working in the dir it names")
	default:
	}
	select {
	case <-inRoot:
		t.Error("EndUnder with the root ended a sleep working in /")
	default:
	}
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sleepIn starts a sleep in a new session with an empty environment and dir
// as its working directory. The channel closes when the sleep exits.
func sleepIn(t *testing.T, dir string) <-chan struct{} {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(ended)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ended
	})
	return ended
}

// waitGone waits for pid, which this process did not start, to end.
func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s, process %d, still runs 5s later", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startDetaching returns the Process of an exited detach helper and the pid
// of the sleep it left running in dir, with the helper's environment or, when
// env is "empty", none.
func startDetaching(t *testing.T, dir, env string) (*Process, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A helper that misses PROC_TEST_DETACH runs no tests instead of this
	// one, which would start helpers of its own.
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PROC_TEST_DETACH="+env)
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
