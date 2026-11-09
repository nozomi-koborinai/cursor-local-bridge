//go:build darwin

package proc

import (
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The Cursor CLI is a node program, and node starts children with
// posix_spawn on macOS rather than fork.
func TestStopEndsWhatNodeSpawned(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `const c = require("child_process").spawn("/bin/sleep", ["120"], {detached: true, stdio: "ignore", env: {}});
console.log(c.pid);
c.unref();`
	p, err := Start(exec.Command(node, "-e", script), nil)
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
		t.Fatalf("node: %v", p.Explain(err))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("node left no running sleep: kill(%d, 0) = %v", pid, err)
	}

	p.Stop()

	waitGone(t, pid, "a sleep node spawned detached with an empty environment")
}
