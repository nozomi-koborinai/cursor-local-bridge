// Package config loads clb's JSON configuration.
package config

import (
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

// HomeEnv overrides the default home directory, ~/.clb.
const HomeEnv = "CLB_HOME"

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
}

// Webhook configures completion notifications.
type Webhook struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

// DefaultHome returns $CLB_HOME, or ~/.clb when it is unset.
func DefaultHome() (string, error) {
	panic("not implemented")
}

// Load reads <home>/config.json if it exists, rejects unknown fields and
// invalid values, and fills defaults. A missing file yields the defaults.
func Load(home string) (Config, error) {
	panic("not implemented")
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string {
	panic("not implemented")
}

// LogDir holds one NDJSON event log per run, <LogDir>/<run id>.ndjson.
func (c Config) LogDir() string {
	panic("not implemented")
}
