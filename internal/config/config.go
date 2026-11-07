// Package config loads clb's JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
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
	// agent_thought_chunk and available_commands_update from the run log.
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
func Load(home string) (Config, error) {
	path := filepath.Join(home, "config.json")
	cfg, err := readConfig(path, home)
	if err != nil {
		return Config{}, err
	}
	if err := validate(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string {
	return filepath.Join(c.Home, "clb.db")
}

// LogDir holds one NDJSON event log per run, <LogDir>/<run id>.ndjson.
func (c Config) LogDir() string {
	return filepath.Join(c.Home, "logs")
}

func readConfig(path, home string) (Config, error) {
	cfg := defaults(home)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
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
