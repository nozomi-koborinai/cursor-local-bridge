package daemon_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

func TestLaunchRejectsARefThatStartsWithADash(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	_, err := h.call(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Ref: "--detach"})
	assertInvalidMessage(t, err, `ref "--detach" starts with "-"`)
	if runs := h.list(); len(runs) != 0 {
		t.Fatalf("launch created runs: %+v", runs)
	}
	if branches := clbBranches(t, h.repo); branches != "" {
		t.Fatalf("launch created branches:\n%s", branches)
	}
}

func TestLaunchRejectsAnUnknownRef(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	_, err := h.call(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Ref: "no-such-ref"})
	assertInvalidMessage(t, err, `ref "no-such-ref" does not name a commit`)
	if runs := h.list(); len(runs) != 0 {
		t.Fatalf("launch created runs: %+v", runs)
	}
	if branches := clbBranches(t, h.repo); branches != "" {
		t.Fatalf("launch created branches:\n%s", branches)
	}
}

func TestLaunchAcceptsABranch(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	const branch = "topic"
	const name = "topic.txt"
	if err := os.WriteFile(filepath.Join(h.repo, name), []byte("from-topic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{
			"-C", h.repo,
			"-c", "user.name=clb",
			"-c", "user.email=clb@example.com",
			"-c", "commit.gpgsign=false",
		}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("checkout", "-b", branch)
	git("add", name)
	git("commit", "-q", "-m", "topic")
	git("checkout", "main")

	run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Ref: branch})
	if run.BaseRef != branch {
		t.Fatalf("baseRef = %q, want %s", run.BaseRef, branch)
	}
	done := h.waitStatus(run.ID, "finished")
	if done.BaseRef != branch {
		t.Fatalf("finished baseRef = %q, want %s", done.BaseRef, branch)
	}
	body, err := os.ReadFile(filepath.Join(done.Worktree, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-topic\n" {
		t.Fatalf("worktree file = %q", body)
	}
}

func clbBranches(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "branch", "--list", "clb/*")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLaunchRecordsAndDisplaysRequestedModelLabel(t *testing.T) {
	t.Run("launch flag overrides config and cli-config", func(t *testing.T) {
		cursorDir := t.TempDir()
		t.Setenv("CURSOR_CONFIG_DIR", cursorDir)
		cliCfg := `{"selectedModel":{"modelId":"grok-4.7","parameters":[{"id":"reasoning_effort","value":"medium"}]}}`
		if err := os.WriteFile(filepath.Join(cursorDir, "cli-config.json"), []byte(cliCfg), 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, &fakeAgent{})
		if err := os.WriteFile(filepath.Join(h.cfg.Home, "config.json"), []byte(`{"model":"composer-2.5"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		const requested = "claude-opus-5-5-xhigh"
		run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Model: requested})
		if run.Model != requested {
			t.Fatalf("launched model = %q, want %q", run.Model, requested)
		}
		done := h.waitStatus(run.ID, "finished")
		if done.Model != requested {
			t.Fatalf("status model = %q, want %q", done.Model, requested)
		}
		if opts := h.agent.session(0).startOptions(); opts.Model != requested {
			t.Fatalf("start options model = %q, want %q", opts.Model, requested)
		}
		runs := h.list()
		if len(runs) != 1 || runs[0].Model != requested {
			t.Fatalf("list = %+v, want model %q", runs, requested)
		}
	})

	t.Run("config.json model reloads without daemon restart", func(t *testing.T) {
		t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
		h := newHarness(t, &fakeAgent{})
		const configured = "grok-4.6[effort=high,fast=false]"
		if err := os.WriteFile(filepath.Join(h.cfg.Home, "config.json"), []byte(`{"model":"`+configured+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		run := h.launch("hi")
		if run.Model != configured {
			t.Fatalf("launched model = %q, want %q", run.Model, configured)
		}
		done := h.waitStatus(run.ID, "finished")
		if done.Model != configured {
			t.Fatalf("finished model = %q, want %q", done.Model, configured)
		}
		if opts := h.agent.session(0).startOptions(); opts.Model != configured {
			t.Fatalf("start options model = %q, want %q", opts.Model, configured)
		}
	})

	t.Run("cli-config.json model and effort recorded when clb model is unset", func(t *testing.T) {
		cursorDir := t.TempDir()
		t.Setenv("CURSOR_CONFIG_DIR", cursorDir)
		cliCfg := `{
			"model": {"modelId": "claude-opus-5-5-high", "displayModelId": "claude-opus-5-5", "displayName": "Claude Opus 5.5"},
			"selectedModel": {
				"modelId": "claude-opus-5-5",
				"parameters": [
					{"id": "context", "value": "300k"},
					{"id": "effort", "value": "xhigh"},
					{"id": "fast", "value": "false"}
				]
			}
		}`
		if err := os.WriteFile(filepath.Join(cursorDir, "cli-config.json"), []byte(cliCfg), 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, &fakeAgent{})

		const want = "claude-opus-5-5[context=300k,effort=xhigh,fast=false]"
		run := h.launch("hi")
		if run.Model != want {
			t.Fatalf("launched model = %q, want %q", run.Model, want)
		}
		done := h.waitStatus(run.ID, "finished")
		if done.Model != want {
			t.Fatalf("finished model = %q, want %q", done.Model, want)
		}
		if opts := h.agent.session(0).startOptions(); opts.Model != "" {
			t.Fatalf("start options model = %q, want empty so the CLI uses its own config", opts.Model)
		}
		runs := h.list()
		if len(runs) != 1 || runs[0].Model != want {
			t.Fatalf("list = %+v, want model %q", runs, want)
		}
	})

	t.Run("backend reports parameterized model and effort on session start", func(t *testing.T) {
		t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
		const reported = "claude-opus-5-5[context=300k,effort=xhigh,fast=false]"
		agent := &fakeAgent{onStart: func(_ context.Context, s *fakeSession) {
			if mr, ok := s.sink.(backend.ModelReporter); ok {
				mr.SetModel(reported)
			}
		}}
		h := newHarness(t, agent)

		run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Model: "claude-opus-5-5"})
		done := h.waitStatus(run.ID, "finished")
		if done.Model != reported {
			t.Fatalf("finished model = %q, want %q", done.Model, reported)
		}
		runs := h.list()
		if len(runs) != 1 || runs[0].Model != reported {
			t.Fatalf("list = %+v, want model %q", runs, reported)
		}
	})
}
