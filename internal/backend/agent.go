package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
)

// versionTimeout bounds "agent --version". Tests shorten it.
var versionTimeout = 5 * time.Second

// maxVersionLen bounds what a run record keeps of --version output.
const maxVersionLen = 64

// StartAgent is how backends start a Cursor CLI process, so every process
// is in its run's log with its CLI version and stderr. It runs
// cmd.Path --version with cmd.Dir and cmd.Env before every start, because
// the CLI can update itself between any two starts, including two print-mode
// prompts of one chat. A failed probe is reported in VersionErr and does not
// stop the start. StartAgent returns ctx.Err() if ctx ends during the probe,
// and does not start cmd.
func StartAgent(ctx context.Context, cmd *exec.Cmd, command Command, sink Sink) (*proc.Process, error) {
	version, verr := probeVersion(ctx, cmd)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verr != nil {
		version = ""
	}
	// The stderr callback can run before Start returns. It waits until the
	// process has been reported, so AgentProcess is always first.
	reported := make(chan struct{})
	var pid int
	p, err := proc.Start(cmd, func(l proc.StderrLine) {
		<-reported
		sink.Stderr(Stderr{PID: pid, Line: l.Text, Capped: l.Capped})
	})
	if err != nil {
		return nil, err
	}
	pid = p.PID()
	sink.AgentProcess(AgentProcess{
		PID:        pid,
		Command:    command,
		Version:    version,
		VersionErr: errText(verr),
	})
	close(reported)
	return p, nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// probeVersion runs cmd.Path --version under proc, so the descendants it
// starts end with it, and returns the version it prints on stdout.
func probeVersion(ctx context.Context, cmd *exec.Cmd) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cmd.Err != nil {
		return "", cmd.Err
	}
	probe := exec.Command(cmd.Path, "--version")
	probe.Dir = cmd.Dir
	if cmd.Env != nil {
		probe.Env = append([]string(nil), cmd.Env...)
	}
	p, err := proc.Start(probe, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		p.Stop()
		_ = p.Stdout.Close()
	}()

	type readResult struct {
		b   []byte
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		b, readErr := io.ReadAll(io.LimitReader(p.Stdout, 4<<10))
		ch <- readResult{b, readErr}
	}()

	timer := time.NewTimer(versionTimeout)
	defer timer.Stop()
	var out []byte
	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		out = r.b
	case <-timer.C:
		return "", fmt.Errorf("agent --version did not exit within %s", versionTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-p.Done():
	case <-timer.C:
		return "", fmt.Errorf("agent --version did not exit within %s", versionTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := p.Wait(); err != nil {
		return "", p.Explain(fmt.Errorf("agent --version: %w", err))
	}
	return parseVersion(out)
}

// parseVersion returns the first non-empty line of out, trimmed, when it is
// 1 to maxVersionLen bytes of printable ASCII. Anything else is an error that
// quotes at most maxVersionLen bytes.
func parseVersion(out []byte) (string, error) {
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		s := strings.TrimSpace(string(line))
		if s == "" {
			continue
		}
		if versionOK(s) {
			return s, nil
		}
		return "", fmt.Errorf("agent --version: %s", quoteVersion(s))
	}
	return "", fmt.Errorf("agent --version: %s", quoteVersion(strings.TrimSpace(string(out))))
}

func versionOK(s string) bool {
	if len(s) < 1 || len(s) > maxVersionLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func quoteVersion(s string) string {
	if len(s) > maxVersionLen {
		s = s[:maxVersionLen]
	}
	return fmt.Sprintf("%q", s)
}
