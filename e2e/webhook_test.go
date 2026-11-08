//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/fakeagent/fakeagenttest"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

func TestWebhookCommands(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLB(t, dir)
	agentPath, err := fakeagenttest.Build(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("setup delivers a test and the daemon uses it", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		hooks := newReceiver(t)
		const key = "setup-key-do-not-print"
		rawURL := hooks.url + "/setup-secret-path"
		writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
			"agentPath":      agentPath,
			"storeAllEvents": true,
		})
		daemon := startDaemon(t, c, t.TempDir())
		pid := daemon.cmd.Process.Pid

		cmd := c.command("webhook", "setup", "--test")
		cmd.Stdin = strings.NewReader(rawURL + "\n" + key + "\n")
		stdout, stderr, code := runCLB(t, cmd)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		var got setupOutput
		decode(t, []byte(stdout), &got)
		host := mustHost(t, hooks.url)
		if got.Config != filepath.Join(c.home, "config.json") || got.WebhookHost != host || !got.SecretCreated || got.Test == nil || got.Test.HTTPStatus != http.StatusNoContent {
			t.Fatalf("stdout = %s", stdout)
		}

		saved := readSavedConfig(t, c.home)
		if saved.AgentPath != agentPath || !saved.StoreAllEvents || saved.Webhook.URL != rawURL {
			t.Fatalf("config = %+v", saved)
		}
		if saved.Webhook.Headers["Authorization"] != "Bearer "+key {
			t.Fatalf("Authorization = %q", saved.Webhook.Headers["Authorization"])
		}
		assertMode(t, filepath.Join(c.home, "config.json"), 0o600)
		assertNoSecret(t, stdout+stderr, key, saved.Webhook.Secret, "/setup-secret-path")

		testDelivery := hooks.wait(t, "test", "test", 1)[0]
		checkDelivery(t, testDelivery, saved.Webhook.Secret, key)
		if testDelivery.payload.Summary != "Test delivery from clb webhook test. No run changed." || testDelivery.payload.Event != "statusChange" {
			t.Fatalf("test payload = %+v", testDelivery.payload)
		}

		repo := gitRepo(t)
		launched := c.run(t, "launch", "--repo", repo, "hello")
		finished := c.waitStatus(t, launched.ID, "finished")
		runDelivery := hooks.wait(t, finished.ID, "finished", 1)[0]
		checkDelivery(t, runDelivery, saved.Webhook.Secret, key)
		if daemon.cmd.Process.Pid != pid || daemon.exited() {
			t.Fatal("the daemon restarted")
		}
	})

	t.Run("hidden input", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		const (
			key    = "pty-hidden-key"
			rawURL = "http://127.0.0.1:9/pty-hidden-path"
		)
		session := c.startPTY(t, "webhook", "setup")
		if !session.echo(t) {
			t.Fatal("pty started with echo disabled")
		}
		session.waitFor(t, "Webhook URL (input hidden): ")
		session.waitEcho(t, false)
		session.write(t, rawURL+"\n")
		session.waitFor(t, "Key (input hidden): ")
		session.waitEcho(t, false)
		session.write(t, key+"\n")
		session.waitFor(t, "secretCreated")
		if err := session.wait(t); err != nil {
			t.Fatalf("exit = %v\n%s", err, session.text())
		}
		transcript := session.text()
		if !strings.Contains(transcript, "Webhook URL (input hidden): ") || !strings.Contains(transcript, "Key (input hidden): ") {
			t.Fatalf("transcript =\n%s", transcript)
		}
		saved := readSavedConfig(t, c.home)
		assertNoSecret(t, transcript, key, saved.Webhook.Secret, "/pty-hidden-path")
		if saved.Webhook.URL != rawURL || saved.Webhook.Headers["Authorization"] != "Bearer "+key {
			t.Fatalf("config = %+v", saved)
		}
	})

	t.Run("interrupt restores echo", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		session := c.startPTY(t, "webhook", "setup")
		if !session.echo(t) {
			t.Fatal("pty started with echo disabled")
		}
		session.waitFor(t, "Webhook URL (input hidden): ")
		session.waitEcho(t, false)
		session.write(t, "https://example.com/interrupt-path\n")
		session.waitFor(t, "Key (input hidden): ")
		session.waitEcho(t, false)
		session.write(t, "\x03")
		err := session.wait(t)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 {
			t.Fatalf("exit = %v\n%s", err, session.text())
		}
		if !session.echo(t) {
			t.Fatal("echo is off after interrupt")
		}
		if _, statErr := os.Stat(filepath.Join(c.home, "config.json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("config.json after interrupt: %v", statErr)
		}
	})

	t.Run("env is idempotent", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		const (
			key    = "env-webhook-key"
			rawURL = "https://example.com/env-secret-path"
		)
		first := runSetupEnv(t, c, rawURL, key)
		if !first.SecretCreated || first.WebhookHost != "example.com" {
			t.Fatalf("first = %+v", first)
		}
		path := filepath.Join(c.home, "config.json")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		second := runSetupEnv(t, c, rawURL, key)
		if second.SecretCreated {
			t.Fatal("secretCreated = true on the second run")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("second file =\n%s\nfirst file =\n%s", after, before)
		}
		saved := readSavedConfig(t, c.home)
		if saved.Webhook.URL != rawURL || saved.Webhook.Headers["Authorization"] != "Bearer "+key {
			t.Fatalf("config = %+v", saved)
		}
	})

	t.Run("bearer prefix", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		cmd := c.command("webhook", "setup")
		cmd.Stdin = strings.NewReader("https://example.com/bearer-path\nbEaReR the-key\n")
		stdout, stderr, code := runCLB(t, cmd)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		assertNoSecret(t, stdout+stderr, "the-key", "/bearer-path")
		saved := readSavedConfig(t, c.home)
		if saved.Webhook.Headers["Authorization"] != "Bearer the-key" {
			t.Fatalf("Authorization = %q", saved.Webhook.Headers["Authorization"])
		}
	})

	t.Run("empty values", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		cmd := c.command("webhook", "setup")
		cmd.Stdin = strings.NewReader("\n")
		_, stderr, code := runCLB(t, cmd)
		if code != 1 || !strings.Contains(stderr, "webhook URL is empty") {
			t.Fatalf("exit %d\n%s", code, stderr)
		}

		cmd = c.command("webhook", "setup")
		cmd.Stdin = strings.NewReader("https://example.com/empty-key-path\n\n")
		_, stderr, code = runCLB(t, cmd)
		if code != 1 || !strings.Contains(stderr, "webhook key is empty") || strings.Contains(stderr, "empty-key-path") {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
	})

	t.Run("test without a url", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		cmd := c.command("webhook", "test")
		_, stderr, code := runCLB(t, cmd)
		if code != 1 || !strings.Contains(stderr, "clb webhook setup") {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
	})

	t.Run("test status 401", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		const key = "receiver-key-do-not-print"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
			"webhook": map[string]any{
				"url":    srv.URL + "/test-401-path",
				"secret": "signing-secret",
				"headers": map[string]string{
					"Authorization": "Bearer " + key,
				},
			},
		})
		cmd := c.command("webhook", "test")
		stdout, stderr, code := runCLB(t, cmd)
		if code != 1 || !strings.Contains(stderr, "401") {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
		}
		assertNoSecret(t, stdout+stderr, key, "/test-401-path")
	})

	t.Run("setup test failure names the saved file", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		const key = "setup-fail-key"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		rawURL := srv.URL + "/setup-fail-path"
		cmd := c.command("webhook", "setup", "--test")
		cmd.Stdin = strings.NewReader(rawURL + "\n" + key + "\n")
		stdout, stderr, code := runCLB(t, cmd)
		configPath := filepath.Join(c.home, "config.json")
		if code != 1 || !strings.Contains(stderr, "401") || !strings.Contains(stderr, "saved "+configPath) {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
		}
		assertNoSecret(t, stdout+stderr, key, "/setup-fail-path")
		if _, err := os.Stat(configPath); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("network error hides the url path", func(t *testing.T) {
		c := &clb{path: bin, home: shortTempDir(t)}
		const key = "closed-port-key"
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		rawURL := "http://" + addr + "/closed-secret-path"
		writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
			"webhook": map[string]any{
				"url":     rawURL,
				"secret":  "signing-secret",
				"headers": map[string]string{"Authorization": "Bearer " + key},
			},
		})
		cmd := c.command("webhook", "test")
		stdout, stderr, code := runCLB(t, cmd)
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		assertNoSecret(t, stdout+stderr, key, "/closed-secret-path", rawURL)
	})
}

type setupOutput struct {
	Config        string `json:"config"`
	WebhookHost   string `json:"webhookHost"`
	SecretCreated bool   `json:"secretCreated"`
	Test          *struct {
		HTTPStatus int `json:"httpStatus"`
	} `json:"test"`
}

type savedConfig struct {
	AgentPath      string `json:"agentPath"`
	StoreAllEvents bool   `json:"storeAllEvents"`
	Webhook        struct {
		URL     string            `json:"url"`
		Secret  string            `json:"secret"`
		Headers map[string]string `json:"headers"`
	} `json:"webhook"`
}

func runSetupEnv(t *testing.T, c *clb, rawURL, key string) setupOutput {
	t.Helper()
	cmd := c.command("webhook", "setup")
	cmd.Env = append(cmd.Env, "CLB_WEBHOOK_URL="+rawURL, "CLB_WEBHOOK_KEY="+key)
	cmd.Stdin = strings.NewReader("")
	stdout, stderr, code := runCLB(t, cmd)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var got setupOutput
	decode(t, []byte(stdout), &got)
	return got
}

func runCLB(t *testing.T, cmd *exec.Cmd) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return out.String(), errb.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	return out.String(), errb.String(), exitErr.ExitCode()
}

func readSavedConfig(t *testing.T, home string) savedConfig {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved savedConfig
	decode(t, body, &saved)
	return saved
}

func checkDelivery(t *testing.T, d delivery, secret, key string) {
	t.Helper()
	if got := d.header.Get("Authorization"); got != "Bearer "+key {
		t.Errorf("Authorization = %q", got)
	}
	if !webhook.Verify([]byte(secret), d.body, d.header.Get(webhook.SignatureHeader)) {
		t.Errorf("signature does not verify for %s", d.body)
	}
}

func assertNoSecret(t *testing.T, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("output contains a secret value\n%s", text)
		}
	}
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

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

type ptySession struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (c *clb) startPTY(t *testing.T, args ...string) *ptySession {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	cmd := c.command(args...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	session := &ptySession{t: t, cmd: cmd, master: master, slave: slave}
	go func() {
		tmp := make([]byte, 1024)
		for {
			n, readErr := master.Read(tmp)
			if n > 0 {
				session.mu.Lock()
				session.buf.Write(tmp[:n])
				session.mu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = master.Close()
		_ = slave.Close()
	})
	return session
}

func (s *ptySession) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *ptySession) write(t *testing.T, text string) {
	t.Helper()
	if _, err := s.master.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
}

func (s *ptySession) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(s.text(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q\n%s", want, s.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *ptySession) echo(t *testing.T) bool {
	t.Helper()
	on, err := echoEnabled(int(s.slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return on
}

func (s *ptySession) waitEcho(t *testing.T, want bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if s.echo(t) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("echo = %v, want %v\n%s", !want, want, s.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *ptySession) wait(t *testing.T) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = s.cmd.Process.Kill()
		t.Fatalf("process did not exit\n%s", s.text())
		return nil
	}
}
