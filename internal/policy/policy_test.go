package policy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/policy"
)

func TestParseAndLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	body := `{
		"allow": {
			"commands": ["pwd", "git status", "git diff *", "rg:*", "go test ./..."],
			"fetch": ["docs.example.com", "*.example.org", "https://api.example.net/v1/*"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := policy.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.IsEmpty() {
		t.Fatal("loaded policy is empty")
	}
	if len(p.Allow.Commands) != 5 || len(p.Allow.Fetch) != 3 {
		t.Fatalf("parsed policy = %+v", p)
	}
}

func TestLoadRepo(t *testing.T) {
	repo := t.TempDir()
	p, err := policy.LoadRepo(repo)
	if err != nil || !p.IsEmpty() {
		t.Fatalf("missing .clb/policy.json = %+v, %v", p, err)
	}

	clbDir := filepath.Join(repo, ".clb")
	if err := os.MkdirAll(clbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clbDir, "policy.json"), []byte(`{"allow":{"commands":["go test ./..."]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = policy.LoadRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if dec := p.Evaluate("execute", "go test ./...", ""); dec.Action != policy.DecisionAllow || dec.Rule != "allow.commands:go test ./..." {
		t.Fatalf("repo policy Evaluate = %+v", dec)
	}
}

func TestParseRejectsInvalidPolicies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "not an object", body: `[]`, want: "JSON object"},
		{name: "null", body: `null`, want: "JSON object"},
		{name: "unknown top-level field", body: `{"mode":"auto"}`, want: "mode"},
		{name: "deny field rejected", body: `{"deny":{"commands":["rm -rf"]}}`, want: "deny"},
		{name: "unknown allow field", body: `{"allow":{"files":["*"]}}`, want: "files"},
		{name: "empty command pattern", body: `{"allow":{"commands":["  "]}}`, want: "policy.allow.commands[0]"},
		{name: "bare wildcard command pattern", body: `{"allow":{"commands":["*"]}}`, want: "policy.allow.commands[0]"},
		{name: "double star command pattern", body: `{"allow":{"commands":["**"]}}`, want: "policy.allow.commands[0]"},
		{name: "star space star command pattern", body: `{"allow":{"commands":["* *"]}}`, want: "policy.allow.commands[0]"},
		{name: "bare colon wildcard command pattern", body: `{"allow":{"commands":[":*"]}}`, want: "policy.allow.commands[0]"},
		{name: "colon double star command pattern", body: `{"allow":{"commands":[":**"]}}`, want: "policy.allow.commands[0]"},
		{name: "command pattern with pipe metasyntax", body: `{"allow":{"commands":["rg foo | head"]}}`, want: "policy.allow.commands[0]"},
		{name: "command pattern with mid-token wildcard", body: `{"allow":{"commands":["git * status"]}}`, want: "policy.allow.commands[0]"},
		{name: "command pattern with shell wrapper", body: `{"allow":{"commands":["bash *"]}}`, want: "policy.allow.commands[0]"},
		{name: "command pattern with eval", body: `{"allow":{"commands":["eval *"]}}`, want: "policy.allow.commands[0]"},
		{name: "empty fetch pattern", body: `{"allow":{"fetch":[""]}}`, want: "policy.allow.fetch[0]"},
		{name: "bare wildcard fetch pattern", body: `{"allow":{"fetch":["*"]}}`, want: "policy.allow.fetch[0]"},
		{name: "double star fetch pattern", body: `{"allow":{"fetch":["**"]}}`, want: "policy.allow.fetch[0]"},
		{name: "tld wildcard fetch pattern", body: `{"allow":{"fetch":["*.com"]}}`, want: "policy.allow.fetch[0]"},
		{name: "public suffix wildcard fetch pattern", body: `{"allow":{"fetch":["*.co.uk"]}}`, want: "policy.allow.fetch[0]"},
		{name: "empty label in host fetch pattern", body: `{"allow":{"fetch":["*..example.com"]}}`, want: "policy.allow.fetch[0]"},
		{name: "ftp fetch pattern", body: `{"allow":{"fetch":["ftp://example.com"]}}`, want: "policy.allow.fetch[0]"},
		{name: "missing fetch host", body: `{"allow":{"fetch":["https:///path"]}}`, want: "policy.allow.fetch[0]"},
		{name: "mid-host wildcard", body: `{"allow":{"fetch":["foo*bar.example.com"]}}`, want: "policy.allow.fetch[0]"},
		{name: "dot-dot in fetch path pattern", body: `{"allow":{"fetch":["https://example.com/a/../b"]}}`, want: "policy.allow.fetch[0]"},
		{name: "mid-path wildcard in fetch pattern", body: `{"allow":{"fetch":["https://example.com/*/items"]}}`, want: "policy.allow.fetch[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := policy.Parse("/path/to/policy.json", []byte(tc.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEvaluateDefaultEmptyPolicyAlwaysAsks(t *testing.T) {
	var empty policy.Policy
	if dec := empty.Evaluate("execute", "pwd", ""); dec.Action != policy.DecisionAsk || dec.Matched || dec.Approve || dec.Rule != "" {
		t.Fatalf("empty policy Evaluate command = %+v, want DecisionAsk", dec)
	}
	if dec := empty.Evaluate("fetch", "", "https://example.com"); dec.Action != policy.DecisionAsk || dec.Matched || dec.Approve || dec.Rule != "" {
		t.Fatalf("empty policy Evaluate fetch = %+v, want DecisionAsk", dec)
	}
}

func TestEvaluateCommandsExactAndExplicitWildcard(t *testing.T) {
	p := policy.Policy{
		Allow: policy.RuleSet{
			Commands: []string{
				"pwd",
				"git status",
				"git diff *",
				"rg:*",
				"go test ./...",
				"python3 -m unittest *",
			},
		},
	}

	tests := []struct {
		name   string
		cmd    string
		action policy.Action
		rule   string
	}{
		{
			name:   "exact single-token command matches",
			cmd:    "pwd",
			action: policy.DecisionAllow,
			rule:   "allow.commands:pwd",
		},
		{
			name:   "exact single-token command does not match with extra arguments",
			cmd:    "pwd -L",
			action: policy.DecisionAsk,
		},
		{
			name:   "exact two-token command matches exact tokens",
			cmd:    "git status",
			action: policy.DecisionAllow,
			rule:   "allow.commands:git status",
		},
		{
			name:   "exact two-token command does not match extra flags",
			cmd:    "git status --short",
			action: policy.DecisionAsk,
		},
		{
			name:   "exact two-token command does not match other subcommand",
			cmd:    "git push origin main",
			action: policy.DecisionAsk,
		},
		{
			name:   "explicit space-star wildcard matches zero trailing args",
			cmd:    "git diff",
			action: policy.DecisionAllow,
			rule:   "allow.commands:git diff *",
		},
		{
			name:   "explicit space-star wildcard matches multiple trailing args",
			cmd:    "git diff --stat HEAD~1 README.md",
			action: policy.DecisionAllow,
			rule:   "allow.commands:git diff *",
		},
		{
			name:   "explicit colon-star wildcard matches simple arguments and single-quoted literal",
			cmd:    "rg -n 'foo && bar | baz' internal/",
			action: policy.DecisionAllow,
			rule:   "allow.commands:rg:*",
		},
		{
			name:   "explicit multi-token command matches exact target",
			cmd:    "go test ./...",
			action: policy.DecisionAllow,
			rule:   "allow.commands:go test ./...",
		},
		{
			name:   "explicit multi-token command without wildcard rejects extra flags",
			cmd:    "go test -exec /tmp/helper ./...",
			action: policy.DecisionAsk,
		},
		{
			name:   "explicit multi-token prefix wildcard matches subcommand args",
			cmd:    "python3 -m unittest discover -s tests",
			action: policy.DecisionAllow,
			rule:   "allow.commands:python3 -m unittest *",
		},
		{
			name:   "explicit multi-token prefix wildcard rejects other python invocation",
			cmd:    "python3 -c 'import os'",
			action: policy.DecisionAsk,
		},
		{
			name:   "partial executable prefix does not match",
			cmd:    "rgrep foo",
			action: policy.DecisionAsk,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Evaluate("execute", tc.cmd, "")
			wantMatched := tc.action == policy.DecisionAllow
			if got.Action != tc.action || got.Matched != wantMatched || got.Approve != wantMatched || got.Rule != tc.rule {
				t.Fatalf("Evaluate(%q) = %+v, want Action=%q Rule=%q", tc.cmd, got, tc.action, tc.rule)
			}
		})
	}
}

func TestEvaluateCommandMetasyntaxAlwaysAsks(t *testing.T) {
	p := policy.Policy{
		Allow: policy.RuleSet{
			Commands: []string{"pwd", "git status", "git diff *", "rg *", "echo *", "go test *"},
		},
	}

	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{name: "pipeline", cmd: "rg foo | head -n 10"},
		{name: "logical and", cmd: "git status && git diff"},
		{name: "logical or", cmd: "go test ./... || echo failed"},
		{name: "semicolon chain", cmd: "pwd; git status"},
		{name: "background operator", cmd: "go test ./... &"},
		{name: "newline chain", cmd: "pwd\ngit status"},
		{name: "carriage return", cmd: "pwd\rgit status"},
		{name: "output redirection", cmd: "echo hello > /tmp/out.txt"},
		{name: "append redirection", cmd: "echo hello >> /tmp/out.txt"},
		{name: "input redirection", cmd: "rg foo < README.md"},
		{name: "fd duplication", cmd: "go test ./... 2>&1"},
		{name: "command substitution $()", cmd: "echo $(id)"},
		{name: "double-quoted command substitution", cmd: `echo "$(id)"`},
		{name: "backtick command substitution", cmd: "echo `id`"},
		{name: "variable expansion", cmd: "echo $HOME"},
		{name: "braced parameter expansion", cmd: "echo ${HOME}"},
		{name: "subshell parentheses", cmd: "(pwd)"},
		{name: "brace group", cmd: "{ pwd; }"},
		{name: "process substitution", cmd: "git diff <(pwd)"},
		{name: "unquoted glob star", cmd: "rg foo *.go"},
		{name: "unquoted question mark glob", cmd: "rg foo ?.go"},
		{name: "unquoted character class glob", cmd: "rg foo [a-z].go"},
		{name: "unquoted tilde expansion", cmd: "rg foo ~/src"},
		{name: "unquoted backslash escape", cmd: `rg foo\ bar`},
		{name: "double-quoted backslash escape", cmd: `echo "hello\n"`},
		{name: "unclosed single quote", cmd: "echo 'hello"},
		{name: "unclosed double quote", cmd: `echo "hello`},
		{name: "leading environment variable assignment", cmd: "GOOS=linux go test ./..."},
		{name: "PATH environment hijack", cmd: "PATH=/tmp/bin rg foo"},
		{name: "eval builtin", cmd: "eval pwd"},
		{name: "source builtin", cmd: "source ./env.sh"},
		{name: "dot builtin", cmd: ". ./env.sh"},
		{name: "exec builtin", cmd: "exec pwd"},
		{name: "bash wrapper", cmd: "bash -c pwd"},
		{name: "env wrapper", cmd: "env pwd"},
		{name: "timeout wrapper", cmd: "timeout 10 pwd"},
		{name: "sudo wrapper", cmd: "sudo pwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Evaluate("execute", tc.cmd, "")
			if got.Action != policy.DecisionAsk || got.Matched || got.Approve {
				t.Fatalf("Evaluate(%q) = %+v, want DecisionAsk", tc.cmd, got)
			}
		})
	}
}

func TestEvaluateFetchPatterns(t *testing.T) {
	p := policy.Policy{
		Allow: policy.RuleSet{
			Fetch: []string{
				"docs.example.com",
				"*.example.org",
				"https://api.example.net/v1/public/*",
				"http://localhost:8080/health",
			},
		},
	}

	tests := []struct {
		name   string
		url    string
		action policy.Action
		rule   string
	}{
		{
			name:   "exact domain matches any clean path",
			url:    "https://docs.example.com/guide/intro?ref=1",
			action: policy.DecisionAllow,
			rule:   "allow.fetch:docs.example.com",
		},
		{
			name:   "wildcard subdomain matches subdomain",
			url:    "https://sub.pkg.example.org/index.html",
			action: policy.DecisionAllow,
			rule:   "allow.fetch:*.example.org",
		},
		{
			name:   "wildcard subdomain does not match bare apex",
			url:    "https://example.org/index.html",
			action: policy.DecisionAsk,
		},
		{
			name:   "full URL prefix wildcard",
			url:    "https://api.example.net/v1/public/items",
			action: policy.DecisionAllow,
			rule:   "allow.fetch:https://api.example.net/v1/public/*",
		},
		{
			name:   "scheme mismatch asks",
			url:    "http://api.example.net/v1/public/items",
			action: policy.DecisionAsk,
		},
		{
			name:   "explicit host and port matches",
			url:    "http://localhost:8080/health",
			action: policy.DecisionAllow,
			rule:   "allow.fetch:http://localhost:8080/health",
		},
		{
			name:   "unapproved port on host without port in rule asks",
			url:    "https://docs.example.com:8443/guide",
			action: policy.DecisionAsk,
		},
		{
			name:   "dot-dot path traversal asks",
			url:    "https://api.example.net/v1/public/../private/items",
			action: policy.DecisionAsk,
		},
		{
			name:   "percent-encoded dot-dot traversal asks",
			url:    "https://api.example.net/v1/public/%2e%2e/private/items",
			action: policy.DecisionAsk,
		},
		{
			name:   "double-encoded dot-dot traversal asks",
			url:    "https://api.example.net/v1/public/%252e%252e/private/items",
			action: policy.DecisionAsk,
		},
		{
			name:   "encoded slash in path asks",
			url:    "https://api.example.net/v1/public/items%2fsecret",
			action: policy.DecisionAsk,
		},
		{
			name:   "URL with userinfo asks",
			url:    "https://user@docs.example.com/guide",
			action: policy.DecisionAsk,
		},
		{
			name:   "unlisted domain asks",
			url:    "https://unlisted.example.invalid/data",
			action: policy.DecisionAsk,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Evaluate("fetch", "", tc.url)
			wantMatched := tc.action == policy.DecisionAllow
			if got.Action != tc.action || got.Matched != wantMatched || got.Approve != wantMatched || got.Rule != tc.rule {
				t.Fatalf("Evaluate(%q) = %+v, want Action=%q Rule=%q", tc.url, got, tc.action, tc.rule)
			}
		})
	}
}
