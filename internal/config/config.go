// Package config loads clb's JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

// HomeEnv overrides the default home directory, ~/.clb.
const HomeEnv = "CLB_HOME"

// MaskedSecret is the placeholder rendered whenever a non-empty webhook secret
// or header value is formatted, logged, or marshaled to JSON.
const MaskedSecret = "********"

// macOS sockaddr_un.sun_path holds 104 bytes, including the terminating NUL.
const socketPathLimit = 104

// Secret holds a webhook credential. The plaintext sits behind a pointer so
// fmt's reflection fallback, which does not call String or Format on an
// unexported field, cannot print it. Reveal is the only way to read it.
type Secret struct {
	p *string
}

// NewSecret returns a Secret holding raw, or the zero Secret when raw is empty.
func NewSecret(raw string) Secret {
	if raw == "" {
		return Secret{}
	}
	v := raw
	return Secret{p: &v}
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool {
	return s.p == nil || *s.p == ""
}

// Reveal returns the raw secret string for signing or sending on the wire.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// Mask returns MaskedSecret when the secret is set, or "" when empty.
func (s Secret) Mask() string {
	if s.IsZero() {
		return ""
	}
	return MaskedSecret
}

func (s Secret) String() string {
	return s.Mask()
}

func (s Secret) GoString() string {
	return fmt.Sprintf("%q", s.Mask())
}

func (s Secret) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(f, "%q", s.Mask())
		return
	}
	_, _ = io.WriteString(f, s.Mask())
}

func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Mask())
}

func (s *Secret) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = Secret{}
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = NewSecret(raw)
	return nil
}

func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.Mask())
}

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
	// StallTimeout fails a queued or running run that receives no agent
	// event within this duration. Waiting in awaiting_permission pauses the
	// timer. Default 0 disables the watchdog.
	StallTimeout Duration `json:"stallTimeout"`
	// KeepAwake holds a power assertion on macOS (caffeinate) while at least
	// one run is active. Default false.
	KeepAwake bool `json:"keepAwake"`
}

// Duration is a time.Duration that unmarshals from a Go duration string such
// as "10m" or "30s". Empty or "0" is zero.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("stallTimeout must be a duration string such as \"10m\" or \"0\"")
	}
	if s == "" || s == "0" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil || parsed < 0 {
		return fmt.Errorf("stallTimeout %q is not a non-negative duration such as \"10m\" or \"0\"", s)
	}
	*d = Duration(parsed)
	return nil
}

// Webhook configures run notifications. Secret is the single canonical
// credential used both to sign X-Clb-Signature and to send
// Authorization: Bearer <secret>.
type Webhook struct {
	URL    string `json:"url"`
	Secret Secret `json:"secret"`
	// Headers are extra request headers sent on every delivery.
	Headers map[string]string `json:"headers"`
	// Reminder is how long a pending permission waits before each
	// permissionReminder webhook. Empty sends none. A Go duration such as "5m".
	Reminder string `json:"reminder,omitempty"`
}

// ReminderDuration returns the parsed positive reminder interval, or 0 when
// Reminder is empty.
func (w Webhook) ReminderDuration() (time.Duration, error) {
	if w.Reminder == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(w.Reminder)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("webhook.reminder %q must be a positive duration", w.Reminder)
	}
	return d, nil
}

type maskedWebhook struct {
	URL      string            `json:"url,omitempty"`
	Secret   string            `json:"secret,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Reminder string            `json:"reminder,omitempty"`
}

func (w Webhook) masked() maskedWebhook {
	var hdrs map[string]string
	if len(w.Headers) > 0 {
		hdrs = make(map[string]string, len(w.Headers))
		for k := range w.Headers {
			hdrs[k] = MaskedSecret
		}
	}
	return maskedWebhook{
		URL:      maskedURL(w.URL),
		Secret:   w.Secret.Mask(),
		Headers:  hdrs,
		Reminder: w.Reminder,
	}
}

// maskedURL keeps the scheme and host. Userinfo, the path, and the query
// can carry a credential, and printed config must not include them.
func maskedURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (w Webhook) String() string {
	b, _ := json.Marshal(w.masked())
	return string(b)
}

func (w Webhook) GoString() string {
	return w.String()
}

func (w Webhook) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(f, "%q", w.String())
		return
	}
	_, _ = io.WriteString(f, w.String())
}

func (w Webhook) MarshalJSON() ([]byte, error) {
	return json.Marshal(w.masked())
}

func (w Webhook) LogValue() slog.Value {
	return slog.AnyValue(w.masked())
}

// StripBearerPrefix trims leading and trailing whitespace and removes one
// leading case-insensitive "Bearer " prefix when present.
func StripBearerPrefix(s string) string {
	s = strings.TrimSpace(s)
	const prefix = "bearer "
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return strings.TrimSpace(s[len(prefix):])
	}
	return s
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
	if err := validate(path, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// SaveWebhook sets webhook.url and webhook.secret in <home>/config.json and
// removes any legacy Authorization entry from webhook.headers. It keeps every
// other field and writes only a file that Load accepts. secretCreated reports
// whether the file had no webhook secret before this call.
func SaveWebhook(home, url, secret string) (secretCreated bool, err error) {
	path := filepath.Join(home, "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return false, err
	}
	next, secretCreated, err := applyWebhook(path, data, url, secret)
	if err != nil {
		return false, err
	}
	cfg, err := parse(path, next, home)
	if err != nil {
		return false, err
	}
	if err := validate(path, &cfg); err != nil {
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
	if err := webhook.CheckHeaders(cfg.Webhook.Headers); err != nil {
		return Config{}, fmt.Errorf("%s: webhook.headers: %w", path, err)
	}
	foldLegacyAuthorization(&cfg.Webhook)
	return cfg, nil
}

func rejectSharedWebhookFile(path string, mode os.FileMode, hook Webhook) error {
	perm := mode.Perm()
	if (!hook.Secret.IsZero() || len(hook.Headers) > 0) && perm&0o077 != 0 {
		return fmt.Errorf("%s: mode %#o lets other users read or change webhook.secret or webhook.headers; run chmod 600 %s", path, perm, path)
	}
	return nil
}

func applyWebhook(path string, data []byte, webhookURL, rawSecret string) ([]byte, bool, error) {
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

	var headers map[string]string
	if raw, ok := hook["headers"]; ok {
		if err := json.Unmarshal(raw, &headers); err != nil || headers == nil {
			return nil, false, fmt.Errorf("%s: webhook.headers must be a JSON object of strings", path)
		}
	}
	hadAuthSecret := false
	for name, val := range headers {
		if textproto.CanonicalMIMEHeaderKey(name) == "Authorization" {
			if StripBearerPrefix(val) != "" {
				hadAuthSecret = true
			}
			delete(headers, name)
		}
	}

	existingSecret := ""
	if raw, ok := hook["secret"]; ok {
		if err := json.Unmarshal(raw, &existingSecret); err != nil {
			return nil, false, fmt.Errorf("%s: webhook.secret must be a string", path)
		}
	}
	secret := StripBearerPrefix(rawSecret)
	if secret == "" {
		return nil, false, fmt.Errorf("%s: webhook.secret is required when webhook.url is set", path)
	}
	secretCreated := existingSecret == "" && !hadAuthSecret

	hookOut := make(map[string]any, len(hook))
	for key, raw := range hook {
		hookOut[key] = raw
	}
	hookOut["url"] = webhookURL
	hookOut["secret"] = secret
	if len(headers) > 0 {
		hookOut["headers"] = headers
	} else {
		delete(hookOut, "headers")
	}

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

func validate(path string, cfg *Config) error {
	if _, err := backend.ParseKind(string(cfg.Backend)); err != nil {
		return fmt.Errorf("%s: backend: %w", path, err)
	}
	if err := validateWebhook(path, &cfg.Webhook); err != nil {
		return err
	}
	if len(cfg.SocketPath) >= socketPathLimit {
		return fmt.Errorf("%s: socketPath is %d bytes, which meets or exceeds the macOS limit of %d; set socketPath to a shorter path", path, len(cfg.SocketPath), socketPathLimit)
	}
	return nil
}

func validateWebhook(path string, hook *Webhook) error {
	if err := webhook.CheckHeaders(hook.Headers); err != nil {
		return fmt.Errorf("%s: webhook.headers: %w", path, err)
	}
	if _, err := hook.ReminderDuration(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	rawSecret := hook.Secret.Reveal()
	if rawSecret == MaskedSecret {
		return fmt.Errorf("%s: webhook.secret is the masked placeholder", path)
	}
	if webhook.ValueHasControl(rawSecret) {
		return fmt.Errorf("%s: webhook.secret has a control character such as CR or LF", path)
	}
	if hook.URL == "" {
		return nil
	}
	parsed, err := url.Parse(hook.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s: webhook.url must be an http or https URL with a host", path)
	}
	if hook.Secret.IsZero() {
		return fmt.Errorf("%s: webhook.secret is required when webhook.url is set", path)
	}
	return nil
}

// foldLegacyAuthorization copies a Bearer token from headers into Secret.
// A legacy file could store a different signing value in secret, and the
// bearer is the key the receiver checks, so it replaces Secret. Any other
// Authorization value stays in headers and overrides the derived bearer.
func foldLegacyAuthorization(hook *Webhook) {
	if len(hook.Headers) == 0 {
		return
	}
	for name, val := range hook.Headers {
		if textproto.CanonicalMIMEHeaderKey(name) != "Authorization" {
			continue
		}
		token, ok := bearerToken(val)
		if !ok {
			continue
		}
		delete(hook.Headers, name)
		hook.Secret = NewSecret(token)
	}
	if len(hook.Headers) == 0 {
		hook.Headers = nil
	}
}

func bearerToken(value string) (string, bool) {
	value = strings.TrimSpace(value)
	const prefix = "bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(value[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
