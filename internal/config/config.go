// Package config loads clb's JSON configuration.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

// HomeEnv overrides the default home directory, ~/.clb.
const HomeEnv = "CLB_HOME"

// macOS sockaddr_un.sun_path holds 104 bytes, including the terminating NUL.
const socketPathLimit = 104

// Config is the parsed contents of <home>/config.json with defaults applied.
type Config struct {
	// Home holds the database, socket, logs, and worktrees. It is not read
	// from the file.
	Home string `json:"-"`
	// AgentPath is the Cursor CLI executable. Default "agent".
	AgentPath string `json:"agentPath"`
	// Backend is used when launch does not name one. Default "acp".
	Backend backend.Kind `json:"backend"`
	// Model is used when launch does not name one. Empty lets the CLI choose.
	Model string `json:"model"`
	// SocketPath is the daemon's unix socket. Default <home>/clb.sock.
	SocketPath string `json:"socketPath"`
	// WorktreeDir holds one worktree per run. Default <home>/worktrees.
	WorktreeDir string `json:"worktreeDir"`
	// Webhook is optional. Secret is required when URL is set.
	Webhook Webhook `json:"webhook"`
	// StoreAllEvents stores every agent event. Default false omits
	// agent_thought_chunk, available_commands_update, and thinking from the run log.
	StoreAllEvents bool `json:"storeAllEvents"`
	// TrustWorktree passes --trust to agent -p when true. Print mode refuses
	// a workspace the CLI has not trusted, and each run uses a new worktree.
	// Default false rejects a print launch or followup instead.
	TrustWorktree bool `json:"trustWorktree"`
}

// Webhook configures completion notifications.
type Webhook struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
	// Headers are extra request headers sent on every delivery.
	Headers map[string]string `json:"headers"`
}

// DefaultHome returns $CLB_HOME, or ~/.clb when it is unset.
func DefaultHome() (string, error) {
	if v := os.Getenv(HomeEnv); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("%s: %w", HomeEnv, err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".clb"), nil
}

// Load reads <home>/config.json if it exists, rejects unknown fields and
// invalid values, and fills defaults. A missing file yields the defaults.
// Load refuses a file other users can access when it holds webhook.secret
// or webhook.headers.
func Load(home string) (Config, error) {
	path := filepath.Join(home, "config.json")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults(home), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg, err := parse(path, data, home)
	if err != nil {
		return Config{}, err
	}
	if err := rejectSharedWebhookFile(path, info.Mode(), cfg.Webhook); err != nil {
		return Config{}, err
	}
	if err := validate(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// SaveWebhook sets webhook.url and the Authorization header in
// <home>/config.json, and webhook.secret when the file has none. It keeps
// every other field and writes only a file that Load accepts.
func SaveWebhook(home, url, authorization string) (secretCreated bool, err error) {
	path := filepath.Join(home, "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return false, err
	}
	next, secretCreated, err := applyWebhook(path, data, url, authorization)
	if err != nil {
		return false, err
	}
	cfg, err := parse(path, next, home)
	if err != nil {
		return false, err
	}
	if err := validate(path, cfg); err != nil {
		return false, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return false, err
	}
	if err := writeConfigAtomic(home, path, next); err != nil {
		return false, err
	}
	return secretCreated, nil
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string {
	return filepath.Join(c.Home, "clb.db")
}

// LogDir holds one NDJSON event log per run, <LogDir>/<run id>.ndjson.
func (c Config) LogDir() string {
	return filepath.Join(c.Home, "logs")
}

func parse(path string, data []byte, home string) (Config, error) {
	cfg := defaults(home)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Home = home
	if cfg.AgentPath == "" {
		cfg.AgentPath = "agent"
	}
	if cfg.Backend == "" {
		cfg.Backend = backend.ACP
	}
	cfg.SocketPath = resolvePath(home, cfg.SocketPath, "clb.sock")
	cfg.WorktreeDir = resolvePath(home, cfg.WorktreeDir, "worktrees")
	return cfg, nil
}

func rejectSharedWebhookFile(path string, mode os.FileMode, hook Webhook) error {
	perm := mode.Perm()
	// 0o077 includes group and other write. Write lets another user point
	// webhook.url at their own server and collect the secret or header values.
	if (hook.Secret != "" || len(hook.Headers) > 0) && perm&0o077 != 0 {
		return fmt.Errorf("%s: mode %#o lets other users read or change webhook.secret or webhook.headers; run chmod 600 %s", path, perm, path)
	}
	return nil
}

// Decoding into Config would write defaults and resolved paths into the file,
// so applyWebhook edits the raw JSON document instead.
func applyWebhook(path string, data []byte, webhookURL, authorization string) ([]byte, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if doc == nil {
		return nil, false, fmt.Errorf("%s: must be a JSON object", path)
	}
	hook := map[string]json.RawMessage{}
	if raw, ok := doc["webhook"]; ok {
		parsed, err := jsonObject(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: webhook must be a JSON object", path)
		}
		hook = parsed
	}

	headers := map[string]string{}
	if raw, ok := hook["headers"]; ok {
		if err := json.Unmarshal(raw, &headers); err != nil || headers == nil {
			return nil, false, fmt.Errorf("%s: webhook.headers must be a JSON object of strings", path)
		}
	}
	for name := range headers {
		if textproto.CanonicalMIMEHeaderKey(name) == "Authorization" {
			delete(headers, name)
		}
	}
	headers["Authorization"] = authorization

	secret := ""
	if raw, ok := hook["secret"]; ok {
		if err := json.Unmarshal(raw, &secret); err != nil {
			return nil, false, fmt.Errorf("%s: webhook.secret must be a string", path)
		}
	}
	secretCreated := false
	if secret == "" {
		var buf [32]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, false, err
		}
		secret = hex.EncodeToString(buf[:])
		secretCreated = true
	}

	hookOut := make(map[string]any, len(hook))
	for key, raw := range hook {
		hookOut[key] = raw
	}
	hookOut["url"] = webhookURL
	hookOut["secret"] = secret
	hookOut["headers"] = headers

	docOut := make(map[string]any, len(doc))
	for key, raw := range doc {
		docOut[key] = raw
	}
	docOut["webhook"] = hookOut

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(docOut); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return buf.Bytes(), secretCreated, nil
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

// CreateTemp starts at mode 0600. Rename keeps that mode, so a config that
// other users could read ends up private.
func writeConfigAtomic(dir, path string, data []byte) (err error) {
	f, err := os.CreateTemp(dir, "config.json.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaults(home string) Config {
	return Config{
		Home:        home,
		AgentPath:   "agent",
		Backend:     backend.ACP,
		SocketPath:  filepath.Join(home, "clb.sock"),
		WorktreeDir: filepath.Join(home, "worktrees"),
	}
}

func resolvePath(home, value, fallback string) string {
	if value == "" {
		value = fallback
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(home, value)
}

func validate(path string, cfg Config) error {
	if _, err := backend.ParseKind(string(cfg.Backend)); err != nil {
		return fmt.Errorf("%s: backend: %w", path, err)
	}
	if err := validateWebhook(path, cfg.Webhook); err != nil {
		return err
	}
	if len(cfg.SocketPath) >= socketPathLimit {
		return fmt.Errorf("%s: socketPath is %d bytes, which meets or exceeds the macOS limit of %d; set socketPath to a shorter path", path, len(cfg.SocketPath), socketPathLimit)
	}
	return nil
}

func validateWebhook(path string, hook Webhook) error {
	if err := webhook.CheckHeaders(hook.Headers); err != nil {
		return fmt.Errorf("%s: webhook.headers: %w", path, err)
	}
	if hook.URL == "" {
		return nil
	}
	parsed, err := url.Parse(hook.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s: webhook.url must be an http or https URL with a host", path)
	}
	if hook.Secret == "" {
		return fmt.Errorf("%s: webhook.secret is required when webhook.url is set", path)
	}
	return nil
}
