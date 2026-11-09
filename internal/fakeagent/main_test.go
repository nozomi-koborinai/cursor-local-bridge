package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionPrintsTheStateFileAndSkipsTheInvocationLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKEAGENT_STATE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("  2026.02.02-fake \nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := captureFD(t, &os.Stdout, func() error { return run([]string{"--version"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026.02.02-fake\n" {
		t.Fatalf("stdout = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "invocations.log")); !os.IsNotExist(err) {
		t.Fatalf("version probe wrote invocations.log: %v", err)
	}
}

func TestVersionDefaultsWhenTheFileIsMissingOrEmpty(t *testing.T) {
	t.Setenv("FAKEAGENT_STATE_DIR", t.TempDir())
	got, err := captureFD(t, &os.Stdout, func() error { return run([]string{"--version"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026.01.01-fake\n" {
		t.Fatalf("stdout = %q", got)
	}

	if err := os.WriteFile(filepath.Join(os.Getenv("FAKEAGENT_STATE_DIR"), "version"), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = captureFD(t, &os.Stdout, func() error { return run([]string{"--version"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026.01.01-fake\n" {
		t.Fatalf("empty file stdout = %q", got)
	}
}

func TestVersionCanFailLikeAnOlderCLI(t *testing.T) {
	t.Setenv("FAKEAGENT_NO_VERSION", "1")
	if _, err := captureFD(t, &os.Stderr, func() error { return run([]string{"--version"}) }); err == nil || err.Error() != "unknown option '--version'" {
		t.Fatalf("run = %v", err)
	}
}

func TestWriteDiagEndsWithoutANewline(t *testing.T) {
	text, err := captureFD(t, &os.Stderr, func() error {
		writeDiag("[stderr] hello")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(text, "\n") {
		t.Fatalf("diag ends with a newline: %q", text[len(text)-20:])
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 3 || lines[0] != "diag: [stderr] hello" || len(lines[1]) <= 4<<10 || lines[2] != "diag: tail" {
		t.Fatalf("diag = %d lines, last %q, long line %d bytes", len(lines), lines[len(lines)-1], len(lines[1]))
	}
}

func captureFD(t *testing.T, fd **os.File, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *fd
	*fd = w
	runErr := fn()
	_ = w.Close()
	*fd = old
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b), runErr
}
