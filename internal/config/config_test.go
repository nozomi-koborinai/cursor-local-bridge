package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

func TestLoadMissingFile(t *testing.T) {
	home := testHome(t)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Home:        home,
		AgentPath:   "agent",
		Backend:     backend.ACP,
		SocketPath:  filepath.Join(home, "clb.sock"),
		WorktreeDir: filepath.Join(home, "worktrees"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.DBPath() != filepath.Join(home, "clb.db") {
		t.Fatalf("db = %s", got.DBPath())
	}
	if got.LogDir() != filepath.Join(home, "logs") {
		t.Fatalf("logs = %s", got.LogDir())
	}

	writeConfig(t, home, "{}\n")
	got, err = Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty object = %+v", got)
	}
}

func TestLoadFullFile(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{
		"agentPath": "/opt/agent",
		"backend": "print",
		"model": "composer",
		"socketPath": "/tmp/clb.sock",
		"worktreeDir": "/tmp/worktrees",
		"webhook": {"url": "https://example.com/hook", "secret": "s3cret"}
	}`)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Home:        home,
		AgentPath:   "/opt/agent",
		Backend:     backend.Print,
		Model:       "composer",
		SocketPath:  "/tmp/clb.sock",
		WorktreeDir: "/tmp/worktrees",
		Webhook:     Webhook{URL: "https://example.com/hook", Secret: "s3cret"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.DBPath() != filepath.Join(home, "clb.db") || got.LogDir() != filepath.Join(home, "logs") {
		t.Fatalf("paths db=%s logs=%s", got.DBPath(), got.LogDir())
	}
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "unknown field", body: `{"theme":"dark"}`, want: "theme"},
		{name: "unknown webhook field", body: `{"webhook":{"nope":true}}`, want: "nope"},
		{name: "bad backend", body: `{"backend":"ssh"}`, want: "backend"},
		{name: "url without secret", body: `{"webhook":{"url":"https://example.com/hook"}}`, want: "webhook.secret"},
		{name: "ftp url", body: `{"webhook":{"url":"ftp://example.com/hook","secret":"x"}}`, want: "webhook.url"},
		{name: "url without host", body: `{"webhook":{"url":"https://","secret":"x"}}`, want: "webhook.url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := testHome(t)
			writeConfig(t, home, tt.body)
			_, err := Load(home)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			file := filepath.Join(home, "config.json")
			if !strings.Contains(msg, file) || !strings.Contains(msg, tt.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLoadSocketPathLength(t *testing.T) {
	home := testHome(t)
	file := filepath.Join(home, "config.json")

	long := "/" + strings.Repeat("s", 103)
	if len(long) != 104 {
		t.Fatalf("len = %d", len(long))
	}
	writeConfig(t, home, fmt.Sprintf(`{"socketPath":%q}`, long))
	_, err := Load(home)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, file) || !strings.Contains(msg, "socketPath") || !strings.Contains(msg, "set socketPath to a shorter path") {
		t.Fatalf("err = %v", err)
	}

	okPath := "/" + strings.Repeat("s", 102)
	if len(okPath) != 103 {
		t.Fatalf("len = %d", len(okPath))
	}
	writeConfig(t, home, fmt.Sprintf(`{"socketPath":%q}`, okPath))
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.SocketPath != okPath {
		t.Fatalf("socket = %s", got.SocketPath)
	}
}

func TestLoadResolvesRelativePaths(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{"socketPath":"run/clb.sock","worktreeDir":"wt","model":"m"}`)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.SocketPath != filepath.Join(home, "run", "clb.sock") {
		t.Fatalf("socket = %s", got.SocketPath)
	}
	if got.WorktreeDir != filepath.Join(home, "wt") {
		t.Fatalf("worktrees = %s", got.WorktreeDir)
	}
	if got.AgentPath != "agent" || got.Backend != backend.ACP || got.Model != "m" || got.Home != home {
		t.Fatalf("cfg = %+v", got)
	}
}

func TestDefaultHomeHonorsCLBHome(t *testing.T) {
	abs := testHome(t)
	t.Setenv(HomeEnv, abs)
	got, err := DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Fatalf("absolute = %s, want %s", got, abs)
	}

	dir := testHome(t)
	t.Chdir(dir)
	t.Setenv(HomeEnv, filepath.Join("rel", "clb"))
	got, err = DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "rel", "clb")
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("relative = %s, want %s", got, want)
	}

	t.Setenv(HomeEnv, "")
	got, err = DefaultHome()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, ".clb") {
		t.Fatalf("default = %s", got)
	}
}

func testHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "clb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
