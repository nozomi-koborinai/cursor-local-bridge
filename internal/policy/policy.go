// Package policy loads and evaluates explicit permission auto-approval rules.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Action is the policy outcome for a permission request.
type Action string

const (
	// DecisionAsk means the request did not match any allowlist rule (or
	// used shell metasyntax that requires human review), so the run waits in
	// awaiting_permission.
	DecisionAsk Action = "ask"
	// DecisionAllow means the request matched an explicit allowlist rule and
	// is approved automatically with allow_once.
	DecisionAllow Action = "allow"
)

// RuleSet lists explicit allow patterns for shell commands and web fetches.
type RuleSet struct {
	Commands []string `json:"commands,omitempty"`
	Fetch    []string `json:"fetch,omitempty"`
}

// Policy configures automatic permission approvals. The default Policy is
// empty and auto-approves nothing. Only requests that match an explicit rule
// in Allow return DecisionAllow; every other request returns DecisionAsk so
// the run waits in awaiting_permission for human review.
type Policy struct {
	Allow RuleSet `json:"allow,omitempty"`
}

// Decision is the result of evaluating a permission request against a Policy.
type Decision struct {
	Action  Action
	Matched bool
	Approve bool
	Rule    string
}

func askDecision() Decision {
	return Decision{Action: DecisionAsk, Matched: false, Approve: false}
}

func allowDecision(rule string) Decision {
	return Decision{Action: DecisionAllow, Matched: true, Approve: true, Rule: rule}
}

// IsEmpty reports whether p has no rules.
func (p Policy) IsEmpty() bool {
	return len(p.Allow.Commands) == 0 && len(p.Allow.Fetch) == 0
}

// Merge returns a Policy whose allowlist rules are the union of p and other.
func (p Policy) Merge(other Policy) Policy {
	if p.IsEmpty() {
		return other
	}
	if other.IsEmpty() {
		return p
	}
	return Policy{
		Allow: RuleSet{
			Commands: append(slices.Clone(p.Allow.Commands), other.Allow.Commands...),
			Fetch:    append(slices.Clone(p.Allow.Fetch), other.Allow.Fetch...),
		},
	}
}

// LoadFile reads, parses, and validates a JSON policy file at path. Unknown
// fields and empty or overly broad patterns are rejected.
func LoadFile(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	return Parse(path, data)
}

// LoadRepo reads <repo>/.clb/policy.json if it exists. A missing file returns
// an empty Policy and nil error.
func LoadRepo(repo string) (Policy, error) {
	if repo == "" {
		return Policy{}, nil
	}
	p, err := LoadFile(filepath.Join(repo, ".clb", "policy.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Policy{}, nil
	}
	return p, err
}

// Parse decodes and validates a JSON policy document.
func Parse(path string, data []byte) (Policy, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := raw.(map[string]any); !ok {
		return Policy{}, fmt.Errorf("%s: policy must be a JSON object", path)
	}
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Validate checks that every allowlist pattern in p is non-empty and
// well-formed.
func (p Policy) Validate() error {
	for i, pat := range p.Allow.Commands {
		if _, err := parseCommandPattern(pat); err != nil {
			return fmt.Errorf("policy.allow.commands[%d]: %w", i, err)
		}
	}
	for i, pat := range p.Allow.Fetch {
		if err := validateFetchPattern(pat); err != nil {
			return fmt.Errorf("policy.allow.fetch[%d]: %w", i, err)
		}
	}
	return nil
}

type commandPattern struct {
	prefix   []string
	wildcard bool
}

// parseCommandPattern parses a command allowlist pattern. A pattern is either:
//   - an exact simple command (such as "pwd" or "git status"), which matches
//     only that exact argv token sequence, or
//   - an explicit prefix wildcard ending in " *" or ":*" (such as "go test *"
//     or "git diff:*"), which matches that token prefix followed by zero or
//     more simple arguments.
//
// Bare "*" or ":*", shell metasyntax, environment assignments, and shell
// evaluation builtins are rejected.
func parseCommandPattern(pat string) (commandPattern, error) {
	trimmed := strings.TrimSpace(pat)
	if trimmed == "" {
		return commandPattern{}, errors.New("pattern must not be empty")
	}
	if trimmed == "*" || trimmed == ":*" {
		return commandPattern{}, errors.New("wildcard pattern must name a command prefix")
	}
	wildcard := false
	switch {
	case strings.HasSuffix(trimmed, ":*"):
		wildcard = true
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ":*"))
	case strings.HasSuffix(trimmed, "*") && len(trimmed) > 1 && (trimmed[len(trimmed)-2] == ' ' || trimmed[len(trimmed)-2] == '\t'):
		wildcard = true
		trimmed = strings.TrimSpace(trimmed[:len(trimmed)-1])
	}
	if trimmed == "" {
		return commandPattern{}, errors.New("wildcard pattern must name a command prefix")
	}
	tokens, ok := parseSimpleCommand(trimmed)
	if !ok || len(tokens) == 0 {
		return commandPattern{}, fmt.Errorf("pattern %q must be a single simple command without shell metasyntax or wildcards", pat)
	}
	return commandPattern{prefix: tokens, wildcard: wildcard}, nil
}

func validateFetchPattern(pat string) error {
	trimmed := strings.TrimSpace(pat)
	if trimmed == "" {
		return errors.New("pattern must not be empty")
	}
	if strings.Contains(trimmed, "*") && strings.Trim(trimmed, "*:/") == "" {
		return errors.New("pattern must name a host or domain")
	}
	rest := trimmed
	if strings.Contains(rest, "://") {
		scheme, afterScheme, _ := strings.Cut(rest, "://")
		scheme = strings.ToLower(scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("pattern %q must use http or https", pat)
		}
		rest = afterScheme
	}
	host, patPath, hasPath := strings.Cut(rest, "/")
	if err := validateHostPattern(host); err != nil {
		return fmt.Errorf("pattern %q: %w", pat, err)
	}
	if hasPath {
		if err := validateFetchPathPattern("/" + patPath); err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
	}
	return nil
}

func validateHostPattern(host string) error {
	if host == "" {
		return errors.New("missing host")
	}
	if strings.ContainsAny(host, " \t\r\n/@") {
		return fmt.Errorf("invalid host pattern %q", host)
	}
	wildcard := strings.HasPrefix(host, "*.")
	h := strings.TrimPrefix(host, "*.")
	if h == "" || strings.Contains(h, "*") {
		return fmt.Errorf("invalid host pattern %q", host)
	}
	hostOnly := h
	if name, port, ok := strings.Cut(h, ":"); ok {
		if name == "" || port == "" {
			return fmt.Errorf("invalid host pattern %q", host)
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return fmt.Errorf("invalid port in host pattern %q", host)
			}
		}
		hostOnly = name
	}
	labels := strings.Split(strings.ToLower(hostOnly), ".")
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("invalid host pattern %q", host)
		}
	}
	if wildcard {
		// Require at least two domain labels after "*." (for example,
		// "*.example.com", not "*.com") and reject common two-label public
		// suffixes such as "*.co.uk".
		if len(labels) < 2 {
			return fmt.Errorf("wildcard host pattern %q must include a registrable domain", host)
		}
		if len(labels) == 2 && len(labels[1]) == 2 && isSecondLevelPublicSuffix(labels[0]) {
			return fmt.Errorf("wildcard host pattern %q must include a registrable domain", host)
		}
	}
	return nil
}

func isSecondLevelPublicSuffix(label string) bool {
	switch label {
	case "co", "com", "org", "net", "gov", "edu", "ac", "ne", "or", "go", "lg", "ad", "ed", "gr":
		return true
	default:
		return false
	}
}

func validateFetchPathPattern(patPath string) error {
	if strings.ContainsAny(patPath, " \t\r\n\\?") || strings.Contains(patPath, "%") {
		return errors.New("fetch path pattern must not contain query strings, percent-encoding, or whitespace")
	}
	trimmed := strings.TrimSuffix(patPath, "*")
	if strings.Contains(trimmed, "*") {
		return errors.New("fetch path wildcard '*' is only allowed at the end of the path")
	}
	if hasDotSegment(trimmed) {
		return errors.New("fetch path pattern must not contain . or .. segments")
	}
	return nil
}

// Evaluate checks a permission request with kind, command, and rawURL against
// p. Only explicit allowlist matches return DecisionAllow; everything else
// returns DecisionAsk.
func (p Policy) Evaluate(kind, command, rawURL string) Decision {
	if p.IsEmpty() {
		return askDecision()
	}
	if command != "" && (kind == "" || kind == "execute") {
		return p.evalCommand(command)
	}
	if rawURL != "" && (kind == "" || kind == "fetch") {
		return p.evalFetch(rawURL)
	}
	return askDecision()
}

func (p Policy) evalCommand(command string) Decision {
	if len(p.Allow.Commands) == 0 {
		return askDecision()
	}
	argv, ok := parseSimpleCommand(command)
	if !ok || len(argv) == 0 {
		return askDecision()
	}
	for _, rawPat := range p.Allow.Commands {
		pat, err := parseCommandPattern(rawPat)
		if err != nil {
			continue
		}
		if matchSimpleCommand(pat, argv) {
			return allowDecision("allow.commands:" + rawPat)
		}
	}
	return askDecision()
}

func matchSimpleCommand(pat commandPattern, argv []string) bool {
	if !pat.wildcard {
		return slices.Equal(argv, pat.prefix)
	}
	if len(argv) < len(pat.prefix) {
		return false
	}
	return slices.Equal(argv[:len(pat.prefix)], pat.prefix)
}

// parseSimpleCommand parses s as a single simple command (argv words) without
// shell metasyntax. It returns ok == false whenever s is empty or contains:
//   - pipelines or compound operators (|, ||, &&, ;, &, newlines)
//   - redirections (>, >>, <, <<, <<<, >&, <&, <>)
//   - expansions or subshells ($(), “, ${}, $VAR, (), {}, <(), >())
//   - unquoted globbing or special characters (*, ?, [, ], ~, \, #)
//   - double-quoted escapes or expansions ($, `, \ inside "...")
//   - leading environment variable assignments (VAR=value)
//   - shell evaluation builtins or command wrappers as the executable
//     (eval, source, ., exec, command, builtin, sh, bash, zsh, dash, ksh, env, sudo, etc.)
func parseSimpleCommand(s string) (argv []string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != ' ' && c != '\t') || c == 0x7f {
			return nil, false
		}
	}

	var cur strings.Builder
	var inToken, inSingle, inDouble bool

	flush := func() {
		if !inToken {
			return
		}
		argv = append(argv, cur.String())
		cur.Reset()
		inToken = false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
				continue
			}
			cur.WriteByte(c)
			continue
		}
		if inDouble {
			switch c {
			case '"':
				inDouble = false
			case '$', '`', '\\':
				return nil, false
			default:
				cur.WriteByte(c)
			}
			continue
		}

		switch c {
		case ' ', '\t':
			flush()
		case '\'':
			inSingle = true
			inToken = true
		case '"':
			inDouble = true
			inToken = true
		case '|', '&', ';', '>', '<', '$', '`', '(', ')', '{', '}', '*', '?', '[', ']', '\\':
			return nil, false
		case '~':
			if !inToken || strings.HasSuffix(cur.String(), "=") || strings.HasSuffix(cur.String(), ":") {
				return nil, false
			}
			cur.WriteByte(c)
		case '#':
			if !inToken {
				return nil, false
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
			inToken = true
		}
	}
	if inSingle || inDouble {
		return nil, false
	}
	flush()
	if len(argv) == 0 || argv[0] == "" {
		return nil, false
	}
	if isEnvAssignment(argv[0]) || isDisallowedCommand(argv[0]) {
		return nil, false
	}
	return argv, true
}

func isEnvAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return false
	}
	name := token[:eq]
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' {
				return false
			}
			continue
		}
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func isDisallowedCommand(cmd string) bool {
	switch filepath.Base(cmd) {
	case "eval", "source", ".", "exec", "command", "builtin",
		"sh", "bash", "zsh", "dash", "ksh", "csh", "tcsh", "fish",
		"env", "sudo", "doas", "su", "nohup", "nice", "time", "timeout", "stdbuf", "xargs", "watch":
		return true
	default:
		return false
	}
}

func (p Policy) evalFetch(rawURL string) Decision {
	if len(p.Allow.Fetch) == 0 {
		return askDecision()
	}
	u, err := parseHTTPURL(rawURL)
	if err != nil || hasAmbiguousURLPath(u) {
		return askDecision()
	}
	for _, pat := range p.Allow.Fetch {
		if matchFetchAllow(pat, u) {
			return allowDecision("allow.fetch:" + pat)
		}
	}
	return askDecision()
}

func parseHTTPURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("invalid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("unsupported scheme")
	}
	return u, nil
}

func hasAmbiguousURLPath(u *url.URL) bool {
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") ||
		strings.Contains(escaped, "%5c") ||
		strings.Contains(escaped, "%00") ||
		strings.Contains(escaped, "%2e") ||
		strings.Contains(escaped, "%25") ||
		strings.Contains(u.Path, "\\") ||
		strings.Contains(u.Path, "%") {
		return true
	}
	return hasDotSegment(u.Path) || hasDotSegment(u.EscapedPath())
}

func hasDotSegment(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func cleanURLPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := path.Clean("/" + p)
	if cleaned != "/" && strings.HasSuffix(p, "/") {
		cleaned += "/"
	}
	return cleaned
}

func matchFetchAllow(pat string, u *url.URL) bool {
	if validateFetchPattern(pat) != nil {
		return false
	}
	pat = strings.TrimSpace(pat)
	reqScheme := strings.ToLower(u.Scheme)
	reqHost := strings.ToLower(u.Hostname())
	reqHostPort := strings.ToLower(u.Host)
	reqPath := cleanURLPath(u.Path)

	var patScheme string
	rest := pat
	if strings.Contains(rest, "://") {
		patScheme, rest, _ = strings.Cut(rest, "://")
		patScheme = strings.ToLower(patScheme)
		if patScheme != reqScheme {
			return false
		}
	}

	patHost, patPath, hasPath := strings.Cut(rest, "/")
	patHost = strings.ToLower(patHost)
	if !matchHost(patHost, reqHost, reqHostPort, u.Port()) {
		return false
	}
	if !hasPath {
		return true
	}
	patPath = "/" + patPath
	if strings.HasSuffix(patPath, "*") {
		prefix := strings.TrimSuffix(patPath, "*")
		normPrefix := cleanURLPath(prefix)
		if normPrefix == "/" {
			return strings.HasPrefix(reqPath, "/")
		}
		if reqPath == strings.TrimSuffix(normPrefix, "/") {
			return true
		}
		return strings.HasPrefix(reqPath, strings.TrimSuffix(normPrefix, "/")+"/")
	}
	normPatPath := cleanURLPath(patPath)
	if reqPath == normPatPath {
		return true
	}
	prefix := strings.TrimSuffix(normPatPath, "/") + "/"
	return strings.HasPrefix(reqPath, prefix)
}

func matchHost(patHost, reqHost, reqHostPort, reqPort string) bool {
	if patHost == "" || patHost == "*" {
		return false
	}
	target := reqHost
	if strings.Contains(patHost, ":") {
		target = reqHostPort
	} else if reqPort != "" {
		return false
	}
	if strings.HasPrefix(patHost, "*.") {
		suffix := patHost[1:] // ".example.com"
		return len(target) > len(suffix) && strings.HasSuffix(target, suffix)
	}
	return target == patHost
}
