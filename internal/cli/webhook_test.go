package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
)

func TestWebhookSetupStoresOneSecretAndMasksOutput(t *testing.T) {
	t.Setenv("CLB_WEBHOOK_URL", "")
	t.Setenv("CLB_WEBHOOK_KEY", "")
	t.Setenv(config.HomeEnv, "")
	home := testHome(t)
	const key = "cli-setup-key"
	stdout, stderr, err := runWebhookSetup(t, home, "https://user:pw@example.com/hook?q=1\n"+key+"\n")
	if err != nil {
		t.Fatal(err)
	}
	combined := stdout + stderr
	for _, leak := range []string{key, "user:pw", "q=1", "/hook"} {
		if strings.Contains(combined, leak) {
			t.Fatalf("output contains %q\nstdout=%s\nstderr=%s", leak, stdout, stderr)
		}
	}
	var got struct {
		WebhookHost   string `json:"webhookHost"`
		Secret        string `json:"secret"`
		SecretCreated bool   `json:"secretCreated"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.WebhookHost != "example.com" || got.Secret != config.MaskedSecret || !got.SecretCreated {
		t.Fatalf("stdout = %s", stdout)
	}
	body := readConfig(t, home)
	if bytes.Count(body, []byte(key)) != 1 || bytes.Contains(body, []byte("Authorization")) {
		t.Fatalf("config =\n%s", body)
	}

	stdout, stderr, err = runWebhookSetup(t, home, "https://example.com/hook\n"+key+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout+stderr, key) {
		t.Fatalf("second output contains the key\n%s%s", stdout, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.SecretCreated || got.Secret != config.MaskedSecret {
		t.Fatalf("second stdout = %s", stdout)
	}
}

func TestWebhookSetupRejectsAControlCharacterWithoutPrintingTheKey(t *testing.T) {
	t.Setenv("CLB_WEBHOOK_URL", "")
	t.Setenv("CLB_WEBHOOK_KEY", "")
	t.Setenv(config.HomeEnv, "")
	home := testHome(t)
	const key = "ctrl-setup-key"
	_, stderr, err := runWebhookSetup(t, home, "https://example.com/hook\n"+key+"\rrest\n")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(stderr, key) {
		t.Fatalf("error includes the key: %v\n%s", err, stderr)
	}
	if _, statErr := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(statErr) {
		t.Fatalf("config.json after a rejected key: %v", statErr)
	}
}

func runWebhookSetup(t *testing.T, home, stdin string) (string, string, error) {
	t.Helper()
	root := newRoot("test")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs([]string{"--home", home, "webhook", "setup"})
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func readConfig(t *testing.T, home string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
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
