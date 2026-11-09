package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

func TestRunText(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 43, 12, 0, time.UTC)
	r := ipc.Run{
		ID:           "run-0123abcd",
		Status:       "failed",
		Repo:         "/path/to/repo",
		BaseRef:      "main",
		Branch:       "clb/run-0123abcd",
		Worktree:     "/path/to/worktree",
		Backend:      "acp",
		AgentVersion: "2026.01.01-abcdef0",
		SessionID:    "5b0c1f9e-2a7d-4f3e-9c41-0d2e8a6b7c13",
		Error:        "prompt agent: boom\n\x1b[31mred\x1b[0m\u009b",
		CreatedAt:    time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC),
		UpdatedAt:    now.Add(-2 * time.Minute),
	}
	got := runText(r, now)
	want := "" +
		"run-0123abcd  failed  (updated 2m ago)\n" +
		"  repo      /path/to/repo\n" +
		"  ref       main\n" +
		"  branch    clb/run-0123abcd\n" +
		"  worktree  /path/to/worktree\n" +
		"  backend   acp\n" +
		"  agent     2026.01.01-abcdef0\n" +
		"  session   5b0c1f9e-2a7d-4f3e-9c41-0d2e8a6b7c13\n" +
		"  created   2026-01-01T09:30:00Z\n" +
		"  updated   2026-01-01T09:41:12Z\n" +
		"  error\n" +
		"    prompt agent: boom\n" +
		"    \\u001B[31mred\\u001B[0m\\u009B\n" +
		"  stderr    clb logs --type stderr run-0123abcd\n"
	if got != want {
		t.Fatalf("text =\n%s\nwant =\n%s", got, want)
	}
}

func TestRunsTable(t *testing.T) {
	now := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("a", 70)
	runs := []ipc.Run{
		{
			ID: "run-0123abcd", Status: "failed", AgentVersion: "2026.01.01-abcdef0",
			Error: "prompt agent: boom", UpdatedAt: now.Add(-2 * time.Minute),
		},
		{
			ID: "run-4567cdef", Status: "running", AgentVersion: "2026.01.01-abcdef0",
			UpdatedAt: now.Add(-14 * time.Minute),
		},
		{
			ID: "run-89abcdef", Status: "finished", AgentVersion: "2025.12.20-0fedcba",
			Summary: "Added a flag.\nsecond line", UpdatedAt: now.Add(-48 * time.Hour),
		},
		{
			ID: "run-aaaaaaaaaaaa", Status: "failed", Error: long,
			UpdatedAt: now.Add(-time.Hour),
		},
	}
	got := runsTable(runs[:3], now)
	want := "" +
		"ID            STATUS    AGENT               UPDATED  NOTE\n" +
		"run-0123abcd  failed    2026.01.01-abcdef0  2m ago   prompt agent: boom\n" +
		"run-4567cdef  running   2026.01.01-abcdef0  14m ago\n" +
		"run-89abcdef  finished  2025.12.20-0fedcba  2d ago   Added a flag.\n"
	if got != want {
		t.Fatalf("table =\n%s\nwant =\n%s", got, want)
	}
	note := noteOf(runs[3])
	if rs := []rune(note); len(rs) != 60 || rs[59] != '…' || string(rs[:59]) != long[:59] {
		t.Fatalf("note = %q (%d runes)", note, len([]rune(note)))
	}
}

func TestPrintableReplacesInvalidUTF8(t *testing.T) {
	got := printable("ok\t\xff")
	if got != "ok\t\uFFFD" {
		t.Fatalf("printable = %q", got)
	}
}

func TestWriteRunJSONKeepsTheIndentedObject(t *testing.T) {
	r := ipc.Run{
		ID: "run-0123abcd", Status: "failed", AgentVersion: "2026.01.01-fake", Error: "boom",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
	}
	var got, want bytes.Buffer
	if err := writeRun(&got, outputJSON, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := writeIndented(&want, r); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("json =\n%s\nwant =\n%s", got.String(), want.String())
	}
	var decoded ipc.Run
	if err := json.Unmarshal(got.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AgentVersion != r.AgentVersion || decoded.Error != r.Error {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestOutputFlagRejectsUnknownFormat(t *testing.T) {
	f := outputJSON
	if err := f.Set("text"); err != nil || f != outputText {
		t.Fatalf("Set(text) = %v, format %q", err, f)
	}
	if err := f.Set("yaml"); err == nil {
		t.Fatal("Set(yaml) succeeded")
	}
	cmd := newRoot("test")
	cmd.SetArgs([]string{"status", "--output", "yaml", "run-0123abcd"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "yaml") {
		t.Fatalf("status --output yaml: %v", err)
	}
}

func TestEventWantedFiltersLogTypes(t *testing.T) {
	if !eventWanted(nil, "stderr") || !eventWanted([]string{"stderr", "agent_process"}, "agent_process") {
		t.Fatal("wanted events were filtered out")
	}
	if eventWanted([]string{"stderr"}, "status") {
		t.Fatal("status passed a stderr filter")
	}
}
