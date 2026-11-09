package keepawake

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunCaffeinatePassesFlagsAndStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-caffeinate")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nsleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = runCaffeinate(ctx, script, 4242)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(argsFile)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			got := strings.Split(strings.TrimSpace(string(b)), "\n")
			want := []string{"-i", "-w", strconv.Itoa(4242)}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("args = %v, want %v", got, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake-caffeinate never wrote args")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runCaffeinate did not return after context cancellation")
	}
}

func TestDefaultMatchesPlatform(t *testing.T) {
	fn := Default()
	if runtime.GOOS == "darwin" && fn == nil {
		t.Fatal("Default() = nil on darwin")
	}
	if runtime.GOOS != "darwin" && fn != nil {
		t.Fatal("Default() != nil on non-darwin")
	}
}
