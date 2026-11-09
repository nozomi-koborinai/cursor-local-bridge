package backend

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/fakeagent/fakeagenttest"
)

func TestStartAgentReportsVersionThenStderr(t *testing.T) {
	bin := buildFake(t)
	state := t.TempDir()
	t.Setenv("FAKEAGENT_STATE_DIR", state)
	const version = "2026.01.08-abcdef0"
	if err := os.WriteFile(filepath.Join(state, "version"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sink := &recordingSink{}
	cmd := exec.Command(bin, "-p", "--trust", "--output-format", "stream-json", "--", "[stderr] hello")
	p, err := StartAgent(context.Background(), cmd, CommandPrint, sink)
	if err != nil {
		t.Fatal(err)
	}
	waitProcess(t, p)

	procs, lines, order := sink.snapshot()
	if len(procs) != 1 {
		t.Fatalf("processes = %+v", procs)
	}
	got := procs[0]
	if got.PID != p.PID() || got.Command != CommandPrint || got.Version != version || got.VersionErr != "" {
		t.Fatalf("process = %+v, want pid %d version %s", got, p.PID(), version)
	}
	if len(order) == 0 || order[0] != "process" {
		t.Fatalf("order = %q, want the process before stderr", order)
	}
	wantLines := []string{
		"diag: [stderr] hello",
		strings.Repeat("x", 4<<10),
		"x",
		"diag: tail",
	}
	if len(lines) != len(wantLines) {
		t.Fatalf("stderr lines = %d, want %d (%s)", len(lines), len(wantLines), quoteStderr(lines))
	}
	for i, line := range lines {
		if line.PID != got.PID || line.Capped || line.Line != wantLines[i] || order[i+1] != "stderr" {
			t.Fatalf("line %d = %+v, order %q", i, line, order)
		}
	}
	invs, err := fakeagenttest.Invocations(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || invs[0].Pid != got.PID || invs[0].Args[0] == "--version" {
		t.Fatalf("invocations = %+v, want one print process and no version probe", invs)
	}
}

func TestStartAgentProbeFailureStillStarts(t *testing.T) {
	bin := buildFake(t)
	state := t.TempDir()
	t.Setenv("FAKEAGENT_STATE_DIR", state)
	t.Setenv("FAKEAGENT_NO_VERSION", "1")

	sink := &recordingSink{}
	cmd := exec.Command(bin, "-p", "--trust", "--output-format", "stream-json", "--", "hello")
	p, err := StartAgent(context.Background(), cmd, CommandPrint, sink)
	if err != nil {
		t.Fatal(err)
	}
	waitProcess(t, p)

	procs, _, _ := sink.snapshot()
	if len(procs) != 1 {
		t.Fatalf("processes = %+v", procs)
	}
	got := procs[0]
	if got.Version != "" || got.PID != p.PID() {
		t.Fatalf("process = %+v", got)
	}
	for _, want := range []string{"agent --version: exit status 1", "unknown option '--version'"} {
		if !strings.Contains(got.VersionErr, want) {
			t.Errorf("VersionErr = %q, want it to contain %q", got.VersionErr, want)
		}
	}
	invs, err := fakeagenttest.Invocations(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("invocations = %+v, want the print process even though --version failed", invs)
	}
}

func TestStartAgentRejectsABadVersionAndStillStarts(t *testing.T) {
	bin, started := writeScriptAgent(t)
	for _, tc := range []struct {
		name, out string
		exit      string
	}{
		{name: "empty", out: "\n"},
		{name: "too long", out: strings.Repeat("v", maxVersionLen+1) + "\n"},
		{name: "not ascii", out: "café\n"},
		{name: "nonzero", out: "nope\n", exit: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			versionFile := filepath.Join(dir, "out")
			marker := filepath.Join(dir, "started")
			if err := os.WriteFile(versionFile, []byte(tc.out), 0o600); err != nil {
				t.Fatal(err)
			}
			exit := tc.exit
			if exit == "" {
				exit = "0"
			}
			sink := &recordingSink{}
			cmd := exec.Command(bin, "acp")
			cmd.Env = append(os.Environ(),
				"VERSION_FILE="+versionFile,
				"VERSION_EXIT="+exit,
				"STARTED="+marker,
			)
			p, err := StartAgent(context.Background(), cmd, CommandACP, sink)
			if err != nil {
				t.Fatal(err)
			}
			waitProcess(t, p)
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("command did not start: %v", err)
			}
			procs, _, _ := sink.snapshot()
			if len(procs) != 1 || procs[0].Version != "" || procs[0].VersionErr == "" {
				t.Fatalf("process = %+v, want an empty version and a version error", procs)
			}
		})
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("building the script agent created the started marker")
	}
}

func TestStartAgentKeepsATrimmedVersion(t *testing.T) {
	bin, _ := writeScriptAgent(t)
	dir := t.TempDir()
	versionFile := filepath.Join(dir, "out")
	marker := filepath.Join(dir, "started")
	if err := os.WriteFile(versionFile, []byte("\n  2026.01.01-fake \nextra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "VERSION_FILE="+versionFile, "STARTED="+marker)
	p, err := StartAgent(context.Background(), cmd, CommandCreateChat, sink)
	if err != nil {
		t.Fatal(err)
	}
	waitProcess(t, p)
	procs, _, _ := sink.snapshot()
	if len(procs) != 1 || procs[0].Version != "2026.01.01-fake" || procs[0].VersionErr != "" || procs[0].Command != CommandCreateChat {
		t.Fatalf("process = %+v", procs)
	}
}

func TestStartAgentCancelledDuringProbe(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agent")
	marker := filepath.Join(dir, "started")
	body := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  sleep 30\n  exit 0\nfi\n: > \"$STARTED\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(script, "acp")
	cmd.Env = append(os.Environ(), "STARTED="+marker)
	done := make(chan error, 1)
	go func() {
		_, err := StartAgent(ctx, cmd, CommandACP, &recordingSink{})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartAgent = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StartAgent did not return after the probe was cancelled")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command started after the probe was cancelled: %v", err)
	}
}

func TestStartAgentProbeTimeoutStillStarts(t *testing.T) {
	old := versionTimeout
	versionTimeout = 200 * time.Millisecond
	t.Cleanup(func() { versionTimeout = old })

	dir := t.TempDir()
	script := filepath.Join(dir, "agent")
	marker := filepath.Join(dir, "started")
	body := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  sleep 30\n  exit 0\nfi\n: > \"$STARTED\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	cmd := exec.Command(script, "acp")
	cmd.Env = append(os.Environ(), "STARTED="+marker)
	p, err := StartAgent(context.Background(), cmd, CommandACP, sink)
	if err != nil {
		t.Fatal(err)
	}
	waitProcess(t, p)
	procs, _, _ := sink.snapshot()
	if len(procs) != 1 || procs[0].Version != "" || !strings.Contains(procs[0].VersionErr, "did not exit") {
		t.Fatalf("process = %+v", procs)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("command did not start after the probe timed out: %v", err)
	}
}

func buildFake(t *testing.T) string {
	t.Helper()
	bin, err := fakeagenttest.Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func writeScriptAgent(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent")
	marker := filepath.Join(dir, "started")
	body := `#!/bin/sh
if [ "$1" = "--version" ]; then
  cat "$VERSION_FILE"
  exit ${VERSION_EXIT:-0}
fi
: > "$STARTED"
exit 0
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, marker
}

func waitProcess(t *testing.T, p *proc.Process) {
	t.Helper()
	errc := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, p.Stdout)
		_ = p.Stdout.Close()
		errc <- err
	}()
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

type recordingSink struct {
	mu    sync.Mutex
	procs []AgentProcess
	lines []Stderr
	order []string
}

func (r *recordingSink) Event(Event) {}

func (r *recordingSink) Permission(context.Context, PermissionRequest) (bool, error) {
	return false, errors.New("unexpected permission")
}

func (r *recordingSink) AgentProcess(p AgentProcess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.procs = append(r.procs, p)
	r.order = append(r.order, "process")
}

func (r *recordingSink) Stderr(l Stderr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, l)
	r.order = append(r.order, "stderr")
}

func (r *recordingSink) snapshot() ([]AgentProcess, []Stderr, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AgentProcess(nil), r.procs...), append([]Stderr(nil), r.lines...), append([]string(nil), r.order...)
}

func quoteStderr(lines []Stderr) string {
	var b strings.Builder
	for _, l := range lines {
		if l.Capped {
			b.WriteString(" capped")
			continue
		}
		b.WriteString(" ")
		if len(l.Line) > 24 {
			b.WriteString(l.Line[:24])
			b.WriteString("...")
			continue
		}
		b.WriteString(l.Line)
	}
	return b.String()
}
