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
		Home:           home,
		AgentPath:      "agent",
		Backend:        backend.ACP,
		SocketPath:     filepath.Join(home, "clb.sock"),
		WorktreeDir:    filepath.Join(home, "worktrees"),
		StoreAllEvents: false,
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
		"webhook": {"url": "https://example.com/hook", "secret": "s3cret"},
		"storeAllEvents": true,
		"trustWorktree": true
	}`)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Home:           home,
		AgentPath:      "/opt/agent",
		Backend:        backend.Print,
		Model:          "composer",
		SocketPath:     "/tmp/clb.sock",
		WorktreeDir:    "/tmp/worktrees",
		Webhook:        Webhook{URL: "https://example.com/hook", Secret: "s3cret"},
		StoreAllEvents: true,
		TrustWorktree:  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.DBPath() != filepath.Join(home, "clb.db") || got.LogDir() != filepath.Join(home, "logs") {
		t.Fatalf("paths db=%s logs=%s", got.DBPath(), got.LogDir())
	}
}

func TestLoadWebhookHeaders(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{
		"webhook": {
			"url": "https://example.com/hook",
			"secret": "s3cret",
			"headers": {"Authorization": "Bearer k", "X-Automation-Key": "k"}
		}
	}`)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Authorization": "Bearer k", "X-Automation-Key": "k"}
	if !reflect.DeepEqual(got.Webhook.Headers, want) {
		t.Fatalf("headers = %#v", got.Webhook.Headers)
	}
	if got.Webhook.URL != "https://example.com/hook" || got.Webhook.Secret != "s3cret" {
		t.Fatalf("webhook = %+v", got.Webhook)
	}

	home = testHome(t)
	writeConfig(t, home, `{"webhook":{"headers":{"Authorization":"Bearer only"}}}`)
	got, err = Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook.URL != "" || got.Webhook.Secret != "" {
		t.Fatalf("webhook = %+v", got.Webhook)
	}
	if !reflect.DeepEqual(got.Webhook.Headers, map[string]string{"Authorization": "Bearer only"}) {
		t.Fatalf("headers = %#v", got.Webhook.Headers)
	}
}

func TestLoadRejectsWebhookHeaders(t *testing.T) {
	home := testHome(t)
	const value = "v4lue-s3cret"
	writeConfig(t, home, `{"webhook":{"headers":{"Bad Name":"`+value+`"}}}`)
	_, err := Load(home)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	file := filepath.Join(home, "config.json")
	if !strings.Contains(msg, file) || !strings.Contains(msg, "webhook.headers") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(msg, value) {
		t.Fatalf("err includes the header value: %v", err)
	}
}

func TestLoadTrustWorktree(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{"trustWorktree": false}`)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrustWorktree {
		t.Fatal("TrustWorktree = true, want false")
	}

	writeConfig(t, home, `{"trustWorktree": true}`)
	got, err = Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TrustWorktree {
		t.Fatal("TrustWorktree = false, want true")
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

func TestLoadRefusesLooseWebhookFile(t *testing.T) {
	const value = "v4lue-s3cret"
	secretBody := `{"webhook":{"secret":"s3cret"}}`
	headerBody := `{"webhook":{"headers":{"Authorization":"Bearer ` + value + `"}}}`

	for _, mode := range []os.FileMode{0o644, 0o640, 0o620, 0o604} {
		t.Run(fmt.Sprintf("secret-%#o", mode), func(t *testing.T) {
			home := testHome(t)
			writeConfigMode(t, home, secretBody, mode)
			_, err := Load(home)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			file := filepath.Join(home, "config.json")
			if !strings.Contains(msg, file) || !strings.Contains(msg, "chmod 600") || !strings.Contains(msg, fmt.Sprintf("%#o", mode)) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(msg, "s3cret") {
				t.Fatalf("err includes the secret: %v", err)
			}
		})
	}

	t.Run("headers-0640", func(t *testing.T) {
		home := testHome(t)
		writeConfigMode(t, home, headerBody, 0o640)
		_, err := Load(home)
		if err == nil {
			t.Fatal("expected error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "chmod 600") || !strings.Contains(msg, "0640") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(msg, value) {
			t.Fatalf("err includes the header value: %v", err)
		}
	})

	t.Run("headers-0600", func(t *testing.T) {
		home := testHome(t)
		writeConfigMode(t, home, headerBody, 0o600)
		got, err := Load(home)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"Authorization": "Bearer " + value}
		if !reflect.DeepEqual(got.Webhook.Headers, want) {
			t.Fatalf("headers = %#v", got.Webhook.Headers)
		}
	})

	t.Run("plain-0644", func(t *testing.T) {
		home := testHome(t)
		writeConfigMode(t, home, `{"model":"m"}`, 0o644)
		got, err := Load(home)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != "m" {
			t.Fatalf("model = %q", got.Model)
		}
	})
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	writeConfigMode(t, home, body, 0o600)
}

func writeConfigMode(t *testing.T, home, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("mode = %#o, want %#o", info.Mode().Perm(), mode)
	}
}
