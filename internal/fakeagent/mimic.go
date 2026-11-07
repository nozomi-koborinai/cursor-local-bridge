package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	acp "github.com/coder/acp-go-sdk"
)

const invalidModel = "invalid-model"

// rejectModel exits the way the real CLI does when --model names a model it
// does not offer: after initialize, without answering authenticate, and with
// the whole model list on stderr.
func rejectModel() {
	names := make([]string, 300)
	for i := range names {
		names[i] = fmt.Sprintf("fake-model-%03d", i+1)
	}
	fmt.Fprintf(os.Stderr, "Cannot use this model: %s. Available models: %s\n", invalidModel, strings.Join(names, ", "))
	os.Exit(1)
}

// background leaves a command running after the turn ends, the way the real
// CLI's backgrounded shell commands do. The command runs in its own session,
// outside the agent's process group, with an empty environment and dir as
// its working directory.
func (a *fakeACP) background(ctx context.Context, sid acp.SessionId, dir string) (string, error) {
	cmd := exec.Command("sleep", "120")
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := os.WriteFile(filepath.Join(stateDir(), "background.pid"), []byte(pid+"\n"), 0o644); err != nil {
		return "", err
	}
	reply := "background: started " + pid
	return reply, a.update(ctx, sid, acp.UpdateAgentMessageText(reply))
}
