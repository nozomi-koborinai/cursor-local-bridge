package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
		Webhook:        Webhook{URL: "https://example.com/hook", Secret: NewSecret("s3cret")},
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

func TestLoadWebhookHeadersAndLegacyMigration(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantURL     string
		wantSecret  string
		wantHeaders map[string]string
	}{
		{
			name:        "canonical secret and custom headers",
			body:        `{"webhook":{"url":"https://example.com/hook","secret":"s3cret","headers":{"X-Automation-Key":"k"}}}`,
			wantURL:     "https://example.com/hook",
			wantSecret:  "s3cret",
			wantHeaders: map[string]string{"X-Automation-Key": "k"},
		},
		{
			name:        "legacy both secret and Authorization with same value",
			body:        `{"webhook":{"url":"https://example.com/hook","secret":"s3cret","headers":{"Authorization":"Bearer s3cret","X-Automation-Key":"k"}}}`,
			wantURL:     "https://example.com/hook",
			wantSecret:  "s3cret",
			wantHeaders: map[string]string{"X-Automation-Key": "k"},
		},
		{
			name:        "legacy both secret and Authorization with different values picks Authorization key",
			body:        `{"webhook":{"url":"https://example.com/hook","secret":"random-generated-hex","headers":{"Authorization":"Bearer receiver-key"}}}`,
			wantURL:     "https://example.com/hook",
			wantSecret:  "receiver-key",
			wantHeaders: nil,
		},
		{
			name:        "legacy Authorization only satisfies url secret requirement",
			body:        `{"webhook":{"url":"https://example.com/hook","headers":{"authorization":"bEaReR legacy-only-key"}}}`,
			wantURL:     "https://example.com/hook",
			wantSecret:  "legacy-only-key",
			wantHeaders: nil,
		},
		{
			name:        "legacy Authorization without url",
			body:        `{"webhook":{"headers":{"Authorization":"Bearer only"}}}`,
			wantURL:     "",
			wantSecret:  "only",
			wantHeaders: nil,
		},
		{
			name:        "non-bearer authorization stays an override",
			body:        `{"webhook":{"url":"https://example.com/hook","secret":"s3cret","headers":{"Authorization":"Basic basic-token","X-Custom":"keep"}}}`,
			wantURL:     "https://example.com/hook",
			wantSecret:  "s3cret",
			wantHeaders: map[string]string{"Authorization": "Basic basic-token", "X-Custom": "keep"},
		},
		{
			name:        "token scheme without a secret stays empty",
			body:        `{"webhook":{"headers":{"Authorization":"Token raw-token"}}}`,
			wantURL:     "",
			wantSecret:  "",
			wantHeaders: map[string]string{"Authorization": "Token raw-token"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := testHome(t)
			writeConfig(t, home, tt.body)
			path := filepath.Join(home, "config.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Load(home)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("Load rewrote config.json:\n%s", after)
			}
			if got.Webhook.URL != tt.wantURL {
				t.Fatalf("url = %q, want %q", got.Webhook.URL, tt.wantURL)
			}
			if got.Webhook.Secret.Reveal() != tt.wantSecret {
				t.Fatalf("secret = %q, want %q", got.Webhook.Secret.Reveal(), tt.wantSecret)
			}
			if !reflect.DeepEqual(got.Webhook.Headers, tt.wantHeaders) {
				t.Fatalf("headers = %#v, want %#v", got.Webhook.Headers, tt.wantHeaders)
			}
		})
	}
}

func TestSecretAndConfigMasking(t *testing.T) {
	const (
		rawSecret = "super-secret-webhook-key-999"
		rawHeader = "custom-header-secret-777"
		userinfo  = "userinfo-token-111"
		query     = "query-token-222"
	)
	cfg := Config{
		Home:        "/path/to/home",
		AgentPath:   "agent",
		Backend:     backend.ACP,
		SocketPath:  "/path/to/home/clb.sock",
		WorktreeDir: "/path/to/home/worktrees",
		Webhook: Webhook{
			URL:     "https://user:" + userinfo + "@example.com/hook?q=" + query,
			Secret:  NewSecret(rawSecret),
			Headers: map[string]string{"X-Custom": rawHeader},
		},
	}

	type unexportedHolder struct {
		secret Secret
		hook   Webhook
	}
	holder := unexportedHolder{secret: cfg.Webhook.Secret, hook: cfg.Webhook}

	secretMap := map[string]Secret{"k": cfg.Webhook.Secret}
	hookMap := map[string]Webhook{"k": cfg.Webhook}
	var slogTextBuf, slogJSONBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&slogTextBuf, nil)).Info("cfg", "secret", cfg.Webhook.Secret, "webhook", cfg.Webhook, "cfg", cfg, "secretMap", secretMap, "hookMap", hookMap)
	slog.New(slog.NewJSONHandler(&slogJSONBuf, nil)).Info("cfg", "secret", cfg.Webhook.Secret, "webhook", cfg.Webhook, "cfg", cfg, "secretMap", secretMap, "hookMap", hookMap)

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hookJSON, err := json.Marshal(cfg.Webhook)
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{
		"Secret.String":      cfg.Webhook.Secret.String(),
		"Secret.GoString":    cfg.Webhook.Secret.GoString(),
		"Secret.%v":          fmt.Sprintf("%v", cfg.Webhook.Secret),
		"Secret.%+v":         fmt.Sprintf("%+v", cfg.Webhook.Secret),
		"Secret.%#v":         fmt.Sprintf("%#v", cfg.Webhook.Secret),
		"Secret.%s":          fmt.Sprintf("%s", cfg.Webhook.Secret),
		"Secret.%q":          fmt.Sprintf("%q", cfg.Webhook.Secret),
		"Webhook.%v":         fmt.Sprintf("%v", cfg.Webhook),
		"Webhook.%+v":        fmt.Sprintf("%+v", cfg.Webhook),
		"Webhook.%#v":        fmt.Sprintf("%#v", cfg.Webhook),
		"Config.%v":          fmt.Sprintf("%v", cfg),
		"Config.%+v":         fmt.Sprintf("%+v", cfg),
		"Config.%#v":         fmt.Sprintf("%#v", cfg),
		"unexported.secret":  fmt.Sprintf("%v %+v %#v %s %q", holder.secret, holder, holder, holder.secret, holder.secret),
		"map.secret":         fmt.Sprintf("%v %+v %#v %s %q", secretMap, secretMap, secretMap, secretMap, secretMap),
		"map.webhook":        fmt.Sprintf("%v %+v %#v %s %q", hookMap, hookMap, hookMap, hookMap, hookMap),
		"Config.MarshalJSON": string(cfgJSON),
		"Webhook.Marshal":    string(hookJSON),
		"slog.Text":          slogTextBuf.String(),
		"slog.JSON":          slogJSONBuf.String(),
	}

	for name, out := range outputs {
		if strings.Contains(out, rawSecret) {
			t.Errorf("%s leaked raw secret: %s", name, out)
		}
		if name == "unexported.secret" {
			continue
		}
		for _, leak := range []string{rawHeader, userinfo, query, "/hook"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s leaked %q: %s", name, leak, out)
			}
		}
		if !strings.Contains(out, MaskedSecret) {
			t.Errorf("%s did not include mask %q: %s", name, MaskedSecret, out)
		}
	}

	var empty Secret
	if empty.String() != "" || empty.Mask() != "" || !empty.IsZero() || empty.Reveal() != "" {
		t.Fatalf("empty secret = %+v", empty)
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

func TestReminderDuration(t *testing.T) {
	d, err := (Webhook{}).ReminderDuration()
	if err != nil || d != 0 {
		t.Fatalf("empty = %s, %v", d, err)
	}
	d, err = (Webhook{Reminder: "5m"}).ReminderDuration()
	if err != nil || d != 5*time.Minute {
		t.Fatalf("5m = %s, %v", d, err)
	}
	if _, err := (Webhook{Reminder: "soon"}).ReminderDuration(); err == nil || !strings.Contains(err.Error(), "webhook.reminder") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadReminder(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{
		"webhook": {"url": "https://example.com/hook", "secret": "s3cret", "reminder": "5m"}
	}`)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook.Reminder != "5m" {
		t.Fatalf("reminder = %q", got.Webhook.Reminder)
	}
	d, err := got.Webhook.ReminderDuration()
	if err != nil || d != 5*time.Minute {
		t.Fatalf("duration = %s, %v", d, err)
	}
}

func TestSaveWebhookKeepsReminder(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{
  "webhook": {"url": "https://example.com/old", "secret": "old-secret", "reminder": "5m"}
}`)
	if _, err := SaveWebhook(home, "https://example.com/hook", "Bearer key"); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook.Reminder != "5m" || got.Webhook.URL != "https://example.com/hook" || got.Webhook.Secret.Reveal() != "key" {
		t.Fatalf("webhook = %+v", got.Webhook)
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
		{name: "masked placeholder secret", body: `{"webhook":{"url":"https://example.com/hook","secret":"********"}}`, want: "webhook.secret"},
		{name: "control char in secret", body: "{\"webhook\":{\"url\":\"https://example.com/hook\",\"secret\":\"ctrl-value-999\\u0000tail\"}}", want: "webhook.secret"},
		{name: "duplicate authorization", body: `{"webhook":{"headers":{"Authorization":"Bearer alpha-token-111","authorization":"Bearer beta-token-222"}}}`, want: `headers "Authorization" and "authorization" name the same header`},
		{name: "basic auth is not a secret", body: `{"webhook":{"url":"https://example.com/hook","headers":{"Authorization":"Basic basic-token"}}}`, want: "webhook.secret"},
		{name: "ftp url", body: `{"webhook":{"url":"ftp://example.com/hook","secret":"x"}}`, want: "webhook.url"},
		{name: "url without host", body: `{"webhook":{"url":"https://","secret":"x"}}`, want: "webhook.url"},
		{name: "bad reminder", body: `{"webhook":{"reminder":"soon"}}`, want: "webhook.reminder"},
		{name: "zero reminder", body: `{"webhook":{"reminder":"0s"}}`, want: "webhook.reminder"},
		{name: "negative reminder", body: `{"webhook":{"reminder":"-1s"}}`, want: "webhook.reminder"},
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
			for _, leak := range []string{"ctrl-value-999", "alpha-token-111", "beta-token-222", "basic-token", MaskedSecret} {
				if strings.Contains(msg, leak) {
					t.Fatalf("err includes %q: %v", leak, err)
				}
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

func TestSaveWebhookCreatesHome(t *testing.T) {
	const (
		key = "k3y-do-not-leak"
		url = "https://example.com/hook?a=1&b=2"
	)
	home := filepath.Join(testHome(t), "missing")
	created, err := SaveWebhook(home, url, key)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("secretCreated = false")
	}
	assertMode(t, home, 0o700)
	path := filepath.Join(home, "config.json")
	assertMode(t, path, 0o600)
	if temps := tempConfigs(t, home); len(temps) != 0 {
		t.Fatalf("temp files left behind: %v", temps)
	}

	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Webhook.URL != url || got.Webhook.Secret.Reveal() != key || len(got.Webhook.Headers) != 0 {
		t.Fatalf("webhook = %+v", got.Webhook)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("agentPath")) || bytes.Contains(body, []byte("socketPath")) || bytes.Contains(body, []byte("worktreeDir")) || bytes.Contains(body, []byte("headers")) || bytes.Contains(body, []byte("Authorization")) || bytes.Contains(body, []byte(`\u0026`)) {
		t.Fatalf("file =\n%s", body)
	}
	if gotCount := bytes.Count(body, []byte(key)); gotCount != 1 {
		t.Fatalf("secret occurrences in config.json = %d, want 1:\n%s", gotCount, body)
	}

	again, err := SaveWebhook(home, url, "Bearer "+key)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("secretCreated = true on the second call")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, second) {
		t.Fatalf("second write =\n%s\nfirst write =\n%s", second, body)
	}
}

func TestSaveWebhookKeepsExistingFieldsAndMigratesLegacyHeaders(t *testing.T) {
	const (
		key    = "new-key"
		secret = "old-secret"
		url    = "https://example.com/hook?a=1&b=2"
	)
	home := testHome(t)
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{
  "agentPath": "/opt/agent",
  "backend": "print",
  "model": "composer",
  "storeAllEvents": true,
  "webhook": {"secret": "` + secret + `", "headers": {"authorization": "Bearer old", "AUTHORIZATION": "Bearer other", "X-Custom": "keep-me"}}
}`
	writeConfigMode(t, home, original, 0o644)

	created, err := SaveWebhook(home, url, "Bearer "+key)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("secretCreated = true")
	}
	assertMode(t, home, 0o755)
	path := filepath.Join(home, "config.json")
	assertMode(t, path, 0o600)

	want := `{
  "agentPath": "/opt/agent",
  "backend": "print",
  "model": "composer",
  "storeAllEvents": true,
  "webhook": {
    "headers": {
      "X-Custom": "keep-me"
    },
    "secret": "new-key",
    "url": "https://example.com/hook?a=1&b=2"
  }
}
`
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("file =\n%s\nwant =\n%s", body, want)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentPath != "/opt/agent" || got.Backend != backend.Print || got.Model != "composer" || !got.StoreAllEvents {
		t.Fatalf("cfg = %+v", got)
	}
	if got.Webhook.Secret.Reveal() != key || got.Webhook.URL != url {
		t.Fatalf("webhook = %+v", got.Webhook)
	}
	wantHeaders := map[string]string{"X-Custom": "keep-me"}
	if !reflect.DeepEqual(got.Webhook.Headers, wantHeaders) {
		t.Fatalf("headers = %#v", got.Webhook.Headers)
	}

	again, err := SaveWebhook(home, url, key)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("secretCreated = true on the second call")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, second) {
		t.Fatalf("second write =\n%s", second)
	}
}

func TestSaveWebhookDropsHeadersWhenOnlyAuthorizationExisted(t *testing.T) {
	home := testHome(t)
	writeConfig(t, home, `{"webhook":{"url":"https://example.com/hook","headers":{"Authorization":"Bearer legacy-key"}}}`)
	created, err := SaveWebhook(home, "https://example.com/hook", "rotated-key")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("secretCreated = true when legacy Authorization already existed")
	}
	body, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "webhook": {
    "secret": "rotated-key",
    "url": "https://example.com/hook"
  }
}
`
	if string(body) != want {
		t.Fatalf("file =\n%s\nwant =\n%s", body, want)
	}
}

func TestSaveWebhookRejectsAndLeavesTheFile(t *testing.T) {
	const (
		key    = "k3y-do-not-leak"
		secret = "s3cret-do-not-leak"
	)
	tests := []struct {
		name string
		body string
		url  string
	}{
		{name: "invalid URL", body: `{"webhook":{"secret":"` + secret + `"}}`, url: "ftp://example.com/hook"},
		{name: "non-object", body: "[]\n", url: "https://example.com/hook"},
		{name: "null", body: "null\n", url: "https://example.com/hook"},
		{name: "unknown field", body: `{"theme":"dark","webhook":{"secret":"` + secret + `"}}`, url: "https://example.com/hook"},
		{name: "webhook array", body: `{"webhook":["nope"]}`, url: "https://example.com/hook"},
		{name: "headers array", body: `{"webhook":{"headers":["nope"],"secret":"` + secret + `"}}`, url: "https://example.com/hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := testHome(t)
			writeConfigMode(t, home, tt.body, 0o644)
			path := filepath.Join(home, "config.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = SaveWebhook(home, tt.url, key)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if strings.Contains(msg, key) || strings.Contains(msg, secret) || strings.Contains(msg, "Bearer "+key) {
				t.Fatalf("err includes a secret: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("file changed:\n%s", after)
			}
			assertMode(t, path, 0o644)
			if temps := tempConfigs(t, home); len(temps) != 0 {
				t.Fatalf("temp files left behind: %v", temps)
			}
		})
	}
	t.Run("missing home", func(t *testing.T) {
		home := filepath.Join(testHome(t), "missing")
		if _, err := SaveWebhook(home, "ftp://example.com/hook", key); err == nil {
			t.Fatal("expected error")
		}
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("home after a rejected save: %v", err)
		}
	})
}

func assertMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %#o, want %#o", path, info.Mode().Perm(), mode)
	}
}

func tempConfigs(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "config.json.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
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
		if got.Webhook.Secret.Reveal() != value || len(got.Webhook.Headers) != 0 {
			t.Fatalf("webhook = %+v", got.Webhook)
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
