package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
)

func TestAgentStderrPrecedesTheFailure(t *testing.T) {
	const (
		version = "2026.01.01-fake"
		line    = "Error: example failure"
		reason  = "prompt agent: agent exited (exit status 1); stderr: " + line
	)
	h := newHarness(t, &fakeAgent{
		onStart: func(_ context.Context, s *fakeSession) { s.reportProcess(version) },
		onPrompt: func(_ context.Context, s *fakeSession, _ string) error {
			s.reportStderr(line)
			return errors.New("agent exited (exit status 1); stderr: " + line)
		},
	})
	run := h.launch("hello")
	failed := h.waitStatus(run.ID, "failed")
	assertFailure(t, h, failed, reason, version)

	events := h.waitLog(run.ID, hasType("webhook"))
	stderrAt := eventIndex(events, "stderr", func(e ipc.Event) bool {
		var d struct {
			Line string `json:"line"`
		}
		_ = json.Unmarshal(e.Data, &d)
		return d.Line == line
	})
	failedAt := eventIndex(events, "status", func(e ipc.Event) bool {
		var d struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(e.Data, &d)
		return d.Status == "failed"
	})
	if stderrAt < 0 || failedAt < 0 || stderrAt > failedAt {
		t.Fatalf("stderr at %d, failed status at %d", stderrAt, failedAt)
	}
	proc := eventData(t, events, "agent_process")
	if !strings.Contains(proc, `"version":"`+version+`"`) || !strings.Contains(proc, `"command":"acp"`) {
		t.Fatalf("agent_process = %s", proc)
	}
	h.waitNDJSONMatchesLogs(run.ID)
}

func TestAgentVersionFollowsTheLatestProcess(t *testing.T) {
	const first, second = "2026.01.01-aaaaaaa", "2026.01.08-bbbbbbb"
	h := newHarness(t, &fakeAgent{
		onStart: func(_ context.Context, s *fakeSession) {
			version := first
			if s.n > 1 {
				version = second
			}
			s.reportProcess(version)
		},
	})
	run := h.launch("hello")
	if done := h.waitStatus(run.ID, "finished"); done.AgentVersion != first {
		t.Fatalf("first version = %q", done.AgentVersion)
	}
	if hook := h.hooks.next(t); hook.Status != "finished" || hook.AgentVersion != first {
		t.Fatalf("first webhook = %+v", hook)
	}

	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "again"})
	done := h.waitStatus(run.ID, "finished")
	if done.AgentVersion != second {
		t.Fatalf("version after follow-up = %q, want %s", done.AgentVersion, second)
	}
	if hook := h.hooks.next(t); hook.Status != "finished" || hook.AgentVersion != second || hook.Error != "" {
		t.Fatalf("follow-up webhook = %+v", hook)
	}
	var versions []string
	for _, e := range h.logs(run.ID) {
		if e.Type != "agent_process" {
			continue
		}
		var d struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, d.Version)
	}
	if len(versions) != 2 || versions[0] != first || versions[1] != second {
		t.Fatalf("agent_process versions = %v", versions)
	}
}

func TestAFailedProbeClearsTheVersion(t *testing.T) {
	h := newHarness(t, &fakeAgent{
		onStart: func(_ context.Context, s *fakeSession) {
			if s.n == 1 {
				s.reportProcess("2026.01.01-fake")
				return
			}
			s.sink.AgentProcess(backend.AgentProcess{
				PID: 4242 + s.n, Command: backend.CommandACP, VersionErr: "agent --version: exit status 1",
			})
		},
	})
	run := h.launch("hello")
	h.waitStatus(run.ID, "finished")
	h.hooks.next(t)
	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "again"})
	done := h.waitStatus(run.ID, "finished")
	if done.AgentVersion != "" {
		t.Fatalf("version after a failed probe = %q, want empty", done.AgentVersion)
	}
	var saw bool
	for _, e := range h.logs(run.ID) {
		if e.Type != "agent_process" || !strings.Contains(string(e.Data), "versionError") {
			continue
		}
		if strings.Contains(string(e.Data), `"version"`) {
			t.Fatalf("failed probe still has a version: %s", e.Data)
		}
		saw = true
	}
	if !saw {
		t.Fatal("no agent_process event recorded the probe failure")
	}
}

func TestFailureReasonReachesStatusLogAndWebhook(t *testing.T) {
	t.Run("worktree", func(t *testing.T) {
		h := newHarness(t, &fakeAgent{}, func(cfg *config.Config) {
			blocker := filepath.Join(cfg.Home, "not-a-directory")
			if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.WorktreeDir = filepath.Join(blocker, "wt")
		})
		run := h.launch("hello")
		failed := h.waitStatus(run.ID, "failed")
		if !strings.HasPrefix(failed.Error, "create worktree: ") {
			t.Fatalf("error = %q", failed.Error)
		}
		assertFailure(t, h, failed, failed.Error, "")
	})

	t.Run("start", func(t *testing.T) {
		h := newHarness(t, &fakeAgent{startErr: errors.New("not logged in")})
		run := h.launch("hello")
		failed := h.waitStatus(run.ID, "failed")
		assertFailure(t, h, failed, "start agent: not logged in", "")
	})

	t.Run("prompt", func(t *testing.T) {
		const version = "2026.01.01-fake"
		h := newHarness(t, &fakeAgent{
			onStart: func(_ context.Context, s *fakeSession) { s.reportProcess(version) },
			onPrompt: func(_ context.Context, s *fakeSession, _ string) error {
				s.reportStderr("Error: example failure")
				return errors.New("agent exited")
			},
		})
		run := h.launch("hello")
		failed := h.waitStatus(run.ID, "failed")
		assertFailure(t, h, failed, "prompt agent: agent exited", version)
	})

	t.Run("recover", func(t *testing.T) {
		const version = "2026.01.01-fake"
		h := newHarness(t, &fakeAgent{
			onStart: func(_ context.Context, s *fakeSession) { s.reportProcess(version) },
			onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
				<-ctx.Done()
				return ctx.Err()
			},
		})
		run := h.launch("work")
		h.waitStatus(run.ID, "running")
		h.stop()
		d := h.newDaemon()
		if err := d.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.serve(d)
		failed := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: run.ID})
		assertFailure(t, h, failed, "daemon restarted while the run was active", version)
		if r, err := h.store.GetRun(context.Background(), store.RunID(run.ID)); err != nil || r.Lifecycle.State != lifecycle.Failed {
			t.Fatalf("stored = %s %v", r.Lifecycle.State, err)
		}
	})

	t.Run("stop removes nothing", func(t *testing.T) {
		const version = "2026.01.01-fake"
		h := newHarness(t, &fakeAgent{
			onStart: func(_ context.Context, s *fakeSession) { s.reportProcess(version) },
			onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
				<-ctx.Done()
				return ctx.Err()
			},
		})
		run := h.launch("work")
		h.waitStatus(run.ID, "running")
		gitFile := filepath.Join(run.Worktree, ".git")
		link, err := os.ReadFile(gitFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(gitFile); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(gitFile, link, 0o600) })

		_, stopErr := h.call(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
		var ie *ipc.Error
		if !errors.As(stopErr, &ie) || !strings.HasPrefix(ie.Message, "remove worktree: ") {
			t.Fatalf("stop = %v", stopErr)
		}
		failed := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: run.ID})
		if failed.Error != ie.Message {
			t.Fatalf("status error = %q, stop error = %q", failed.Error, ie.Message)
		}
		assertFailure(t, h, failed, ie.Message, version)
		if _, err := os.Stat(run.Worktree); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	})
}

func assertFailure(t *testing.T, h *harness, run ipc.Run, reason, version string) {
	t.Helper()
	if run.Status != "failed" || run.Error != reason || run.AgentVersion != version || reason == "" {
		t.Fatalf("status = %+v, want failed %q version %q", run, reason, version)
	}
	events := h.waitLog(run.ID, hasType("webhook"))
	var saw bool
	for _, e := range events {
		if e.Type != "status" {
			continue
		}
		var d struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Status != "failed" {
			continue
		}
		saw = true
		if d.Error != reason {
			t.Fatalf("status event error = %q, want %q", d.Error, reason)
		}
	}
	if !saw {
		t.Fatal("no failed status event")
	}
	hook := h.hooks.next(t)
	hook.Timestamp = hook.Timestamp.UTC()
	if hook.RunID != run.ID || hook.Status != "failed" || hook.Error != reason || hook.AgentVersion != version {
		t.Fatalf("webhook = %+v, want failed %q version %q", hook, reason, version)
	}
}

func eventIndex(events []ipc.Event, typ string, match func(ipc.Event) bool) int {
	for i, e := range events {
		if e.Type == typ && match(e) {
			return i
		}
	}
	return -1
}

func eventData(t *testing.T, events []ipc.Event, typ string) string {
	t.Helper()
	for _, e := range events {
		if e.Type == typ {
			return string(e.Data)
		}
	}
	t.Fatalf("no %s event", typ)
	return ""
}
