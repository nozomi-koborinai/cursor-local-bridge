//go:build e2e

package e2e

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/fakeagent/fakeagenttest"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

const printTrustMessage = `the print backend needs "trustWorktree": true in config.json; agent -p refuses an untrusted workspace, and each run uses a new worktree`

func TestPrintLaunchRejectsUntrustedWorktree(t *testing.T) {
	c, repo := startPrintCLB(t, false)
	stderr, err := commandFailed(t, c, "launch", "--backend", "print", "--repo", repo, "hi")
	if err == nil || !strings.Contains(stderr, "invalid") || !strings.Contains(stderr, printTrustMessage) {
		t.Fatalf("launch err = %v, stderr = %q, want invalid and the trust opt-in", err, stderr)
	}
	var runs []ipc.Run
	decode(t, c.output(t, "", "list"), &runs)
	if len(runs) != 0 {
		t.Fatalf("runs = %+v, want none", runs)
	}
}

func TestPrintLaunchWithTrust(t *testing.T) {
	c, repo := startPrintCLB(t, true)
	launched := c.run(t, "launch", "--backend", "print", "--repo", repo, "hi")
	done := c.waitStatus(t, launched.ID, "finished")
	if done.Summary != "echo: hi" {
		t.Errorf("summary = %q, want %q", done.Summary, "echo: hi")
	}
	var assistant, thinking int
	for _, e := range c.logs(t, launched.ID) {
		switch e.Type {
		case "assistant":
			assistant++
		case "thinking":
			thinking++
		}
	}
	if assistant == 0 || thinking != 0 {
		t.Fatalf("assistant events = %d, thinking events = %d, want the reply stored and thinking omitted", assistant, thinking)
	}
}

func startPrintCLB(t *testing.T, trust bool) (*clb, string) {
	t.Helper()
	bin := t.TempDir()
	c := &clb{path: buildCLB(t, bin), home: shortTempDir(t)}
	agentPath, err := fakeagenttest.Build(bin)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"agentPath": agentPath}
	if trust {
		cfg["trustWorktree"] = true
	}
	writeJSON(t, filepath.Join(c.home, "config.json"), cfg)
	startDaemon(t, c, t.TempDir())
	return c, gitRepo(t)
}

func commandFailed(t *testing.T, c *clb, args ...string) (string, error) {
	t.Helper()
	cmd := c.command(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}
