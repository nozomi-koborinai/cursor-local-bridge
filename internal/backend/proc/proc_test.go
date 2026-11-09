package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if how, ok := os.LookupEnv("PROC_TEST_DETACH"); ok {
		detach(how)
	}
	os.Exit(m.Run())
}

// detach starts a sleep in a new session, prints its pid, and exits, which
// leaves the sleep outside the process group that Stop kills. With how
// "empty" the sleep gets an empty environment, the way the Cursor CLI runs
// terminal commands. With how "shell" a shell in a new session with an empty
// environment starts the sleep, prints its pid, and exits, so the sleep's
// parent is gone right away.
func detach(how string) {
	cmd := exec.Command("sleep", "120")
	if how == "shell" {
		cmd = exec.Command("sh", "-c", "sleep 120 >/dev/null & echo $!")
		cmd.Stdout = os.Stdout
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if how != "inherit" {
		cmd.Env = []string{}
	}
	err := cmd.Start()
	if err == nil && how == "shell" {
		err = cmd.Wait()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if how != "shell" {
		fmt.Println(cmd.Process.Pid)
	}
	os.Exit(0)
}

func TestStopEndsDescendantsThatLeftTheGroup(t *testing.T) {
	for _, how := range []string{"inherit", "empty", "shell"} {
		t.Run(how, func(t *testing.T) {
			if how != "inherit" && runtime.GOOS == "linux" {
				t.Skip("Linux finds the descendants that left the group only by the token in their environment")
			}
			dir := t.TempDir()
			p, grandchild := startDetaching(t, dir, how)
			_, otherGrandchild := startDetaching(t, dir, how)

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
		})
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
	real := t.TempDir()
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(real, parent); err != nil {
		t.Fatal(err)
	}
	run := mkdir(t, real, "run-1")
	_, pid := startDetaching(t, run, "empty")

	EndUnder(filepath.Join(parent, "run-1"))

	waitGone(t, pid, "a sleep working in the dir a symlinked parent names")
}

func TestEndUnderDoesNotFollowASymlinkAtTheDir(t *testing.T) {
	base := t.TempDir()
	elsewhere := t.TempDir()
	link := filepath.Join(base, "run-1")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	ended := sleepIn(t, elsewhere)

	EndUnder(link)

	time.Sleep(time.Second)
	select {
	case <-ended:
		t.Fatal("EndUnder ended a sleep working in the directory a symlink at the path names")
	default:
	}
}

func TestEndUnderMatchesAMissingDirUnderASymlinkedParent(t *testing.T) {
	real := t.TempDir()
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(real, parent); err != nil {
		t.Fatal(err)
	}
	run := mkdir(t, real, "run-1")
	ended := sleepIn(t, run)
	if err := os.Remove(run); err != nil {
		t.Fatal(err)
	}

	EndUnder(filepath.Join(parent, "run-1"))

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("EndUnder left a sleep whose working directory is the removed dir under the resolved parent")
	}
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
// of the sleep it left running in dir the way how names.
func startDetaching(t *testing.T, dir, how string) (*Process, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A helper that misses PROC_TEST_DETACH runs no tests instead of this
	// one, which would start helpers of its own.
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PROC_TEST_DETACH="+how)
	p, err := Start(cmd, nil)
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
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("detach helper left no running sleep: kill(%d, 0) = %v", pid, err)
	}
	return p, pid
}

func TestStopEndsChildrenThatIgnoreSIGTERM(t *testing.T) {
	p, err := Start(exec.Command("sh", "-c", `(trap "" TERM; exec sleep 60) & echo $!; wait`), nil)
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
	p, err := Start(exec.Command("sh", "-c", `echo last words; echo oops >&2; exit 3`), nil)
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
			p, err := Start(cmd, nil)
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

func TestStderrLines(t *testing.T) {
	long := strings.Repeat("a", 10<<10)
	runeLine := "x" + strings.Repeat("é", 3000)
	tests := []struct {
		name string
		raw  string
		want []StderrLine
	}{
		{
			name: "lines split on lf",
			raw:  "a\nb\n",
			want: []StderrLine{{Text: "a"}, {Text: "b"}},
		},
		{
			name: "crlf is one line ending",
			raw:  "a\r\nb\r\n",
			want: []StderrLine{{Text: "a"}, {Text: "b"}},
		},
		{
			name: "unterminated line is flushed before Done",
			raw:  "tail",
			want: []StderrLine{{Text: "tail"}},
		},
		{
			name: "a long line is cut at the limit",
			raw:  long + "\n",
			want: []StderrLine{
				{Text: long[:StderrLineLimit]},
				{Text: long[StderrLineLimit : 2*StderrLineLimit]},
				{Text: long[2*StderrLineLimit:]},
			},
		},
		{
			name: "a long line is cut on a rune boundary",
			raw:  runeLine + "\n",
			want: []StderrLine{
				{Text: "x" + strings.Repeat("é", 2047)},
				{Text: strings.Repeat("é", 953)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectStderr(t, tt.raw)
			if !reflectDeepEqualLines(got, tt.want) {
				t.Fatalf("lines = %s, want %s", quoteLines(got), quoteLines(tt.want))
			}
		})
	}
}

func TestStderrLinesAcrossWrites(t *testing.T) {
	raw := []byte("a\r\n" + strings.Repeat("é", 3000) + "\ntail")
	first := strings.Repeat("é", StderrLineLimit/len("é"))
	rest := strings.Repeat("é", 3000-StderrLineLimit/len("é"))
	want := []StderrLine{{Text: "a"}, {Text: first}, {Text: rest}, {Text: "tail"}}
	for _, size := range []int{1, 3, 4095, len(raw)} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			var got []StderrLine
			log := &stderrLog{onLine: func(l StderrLine) { got = append(got, l) }}
			for i := 0; i < len(raw); i += size {
				end := min(i+size, len(raw))
				if _, err := log.Write(raw[i:end]); err != nil {
					t.Fatal(err)
				}
			}
			log.flush()
			if !reflectDeepEqualLines(got, want) {
				t.Fatalf("lines = %s, want %s", quoteLines(got), quoteLines(want))
			}
		})
	}
}

func TestStderrCapStopsTheLog(t *testing.T) {
	const extra = 100
	got := collectStderr(t, strings.Repeat("z", StderrLogLimit+extra))
	var text int
	var capped int
	for i, line := range got {
		if line.Capped {
			capped++
			if line.Text != "" {
				t.Errorf("capped line %d has text %q", i, line.Text)
			}
			if i != len(got)-1 {
				t.Errorf("capped line at %d of %d", i, len(got))
			}
			continue
		}
		if capped > 0 {
			t.Errorf("line %d arrived after the cap", i)
		}
		text += len(line.Text)
	}
	if capped != 1 {
		t.Fatalf("capped markers = %d, lines = %d", capped, len(got))
	}
	if text != StderrLogLimit {
		t.Errorf("logged %d bytes, want %d", text, StderrLogLimit)
	}
}

func TestExplainUnchangedWhenLinesAreForwarded(t *testing.T) {
	a512 := strings.Repeat("a", 512)
	stderr := a512 + "\n"
	cmd := exec.Command("sh", "-c", `printf '%s' "$STDERR" >&2`)
	cmd.Env = append(os.Environ(), "STDERR="+stderr)
	var got []StderrLine
	p, err := Start(cmd, func(l StderrLine) { got = append(got, l) })
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	_ = p.Stdout.Close()
	want := "failed; stderr: " + a512
	if msg := p.Explain(errors.New("failed")).Error(); msg != want {
		t.Errorf("Explain = %s, want %s", brief(msg), brief(want))
	}
	if len(got) != 1 || got[0].Text != a512 || got[0].Capped {
		t.Errorf("lines = %#v, want the trimmed line", got)
	}
}

func collectStderr(t *testing.T, raw string) []StderrLine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr.bin")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []StderrLine
	p, err := Start(exec.Command("sh", "-c", `cat "$1" >&2`, "sh", path), func(l StderrLine) {
		got = append(got, l)
	})
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, p.Stdout)
		_ = p.Stdout.Close()
		read <- err
	}()
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if p.PID() <= 0 {
		t.Errorf("PID = %d", p.PID())
	}
	return got
}

func reflectDeepEqualLines(got, want []StderrLine) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func quoteLines(lines []StderrLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		if l.Capped {
			parts[i] = "capped"
			continue
		}
		parts[i] = brief(l.Text)
	}
	return strings.Join(parts, ", ")
}

func brief(s string) string {
	if len(s) <= 100 {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%q...%q (%d bytes)", s[:50], s[len(s)-50:], len(s))
}
