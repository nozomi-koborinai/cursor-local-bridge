package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/daemon"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

const timeout = 30 * time.Second

func TestLaunchRunsToFinishedAndNotifies(t *testing.T) {
	h := newHarness(t, &fakeAgent{})

	run := h.launch("hello")
	if !regexp.MustCompile(`^run-[0-9a-f]{12}$`).MatchString(run.ID) {
		t.Errorf("id = %q, want run- and 12 hex digits", run.ID)
	}
	if run.Status != "queued" || run.QueuedPrompts != 1 || run.BaseRef != "HEAD" || run.Backend != "acp" ||
		run.Branch != "clb/"+run.ID || run.Worktree != filepath.Join(h.cfg.WorktreeDir, run.ID) {
		t.Errorf("launched run = %+v", run)
	}

	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "echo: hello" || done.QueuedPrompts != 0 || done.SessionID != "session-1" {
		t.Errorf("finished run = %+v", done)
	}
	if opts := h.agent.session(0).startOptions(); opts.Dir != run.Worktree || opts.ResumeSessionID != "" {
		t.Errorf("start options = %+v", opts)
	}
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Errorf("a finished run keeps its worktree: %v", err)
	}

	hook := h.hooks.next(t)
	want := webhook.Payload{
		Event: webhook.EventStatusChange, RunID: run.ID, Status: "finished",
		Repo: h.repo, Branch: run.Branch, Summary: "echo: hello",
	}
	hook.Timestamp = time.Time{}
	if hook != want {
		t.Errorf("webhook = %+v, want %+v", hook, want)
	}

	events := h.waitLog(run.ID, hasType("webhook"))
	wantTypes := []string{
		"status", "status", "prompt", "agent_message_chunk", "tool_call", "agent_message_chunk", "status", "webhook",
	}
	if got := types(events); !slices.Equal(got, wantTypes) {
		t.Errorf("event types = %v, want %v", got, wantTypes)
	}
	if got := statuses(events); !slices.Equal(got, []string{"queued", "running", "finished"}) {
		t.Errorf("statuses = %v", got)
	}
	if got := string(events[2].Data); got != `{"prompt":"hello"}` {
		t.Errorf("prompt event = %s", got)
	}
	if got := string(events[7].Data); got != `{"status":"finished","delivered":true}` {
		t.Errorf("webhook event = %s", got)
	}
	h.waitNDJSONMatchesLogs(run.ID)
}

func TestFollowUpWhileRunningIsSentAfterTheTurn(t *testing.T) {
	release := make(chan struct{})
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "first" {
			<-release
		}
		return echo(ctx, s, prompt)
	}}
	h := newHarness(t, agent)

	run := h.launch("first")
	h.waitStatus(run.ID, "running")
	queued := h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "second"})
	if queued.Status != "running" || queued.QueuedPrompts != 1 {
		t.Errorf("after follow-up: status %s, queued %d", queued.Status, queued.QueuedPrompts)
	}
	close(release)

	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "echo: second" {
		t.Errorf("summary = %q, want the reply to the follow-up", done.Summary)
	}
	if n := h.agent.count(); n != 1 {
		t.Errorf("agent sessions = %d, want the follow-up to reuse the running one", n)
	}
	if got := h.agent.session(0).sentPrompts(); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("prompts = %v", got)
	}
	if hook := h.hooks.next(t); hook.Status != "finished" || hook.Summary != "echo: second" {
		t.Errorf("webhook = %+v, want one finished webhook after the follow-up", hook)
	}
}

func TestFollowUpAfterFinishResumesTheSession(t *testing.T) {
	h := newHarness(t, &fakeAgent{})

	run := h.launch("hello")
	first := h.waitStatus(run.ID, "finished")
	h.hooks.next(t)
	waitClosed(t, h.agent.session(0))

	resumed := h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "again"})
	if resumed.Status != "queued" {
		t.Errorf("status after follow-up = %s, want queued", resumed.Status)
	}
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "echo: again" {
		t.Errorf("summary = %q", done.Summary)
	}
	if got := h.agent.session(1).startOptions().ResumeSessionID; got != first.SessionID || got == "" {
		t.Errorf("resumed session ID = %q, want %q", got, first.SessionID)
	}
	if hook := h.hooks.next(t); hook.Status != "finished" || hook.Summary != "echo: again" {
		t.Errorf("second webhook = %+v", hook)
	}
}

func TestPermissions(t *testing.T) {
	for _, tc := range []struct {
		method ipc.Method
		want   string
	}{
		{ipc.MethodApprove, "permission: approved"},
		{ipc.MethodDeny, "permission: denied"},
	} {
		t.Run(string(tc.method), func(t *testing.T) {
			h := newHarness(t, &fakeAgent{onPrompt: askPermission})

			run := h.launch("[permission]")
			waiting := h.waitStatus(run.ID, "awaiting_permission")
			if p := waiting.PendingPermission; p == nil || p.ID != "perm-1" || p.Title != "Run fake command" {
				t.Fatalf("pending permission = %+v", p)
			}
			answered := h.must(ipc.Request{Method: tc.method, RunID: run.ID})
			if answered.Status != "running" || answered.PendingPermission != nil {
				t.Errorf("after %s: status %s, pending %+v", tc.method, answered.Status, answered.PendingPermission)
			}
			if done := h.waitStatus(run.ID, "finished"); done.Summary != tc.want {
				t.Errorf("summary = %q, want %q", done.Summary, tc.want)
			}
			events := h.logs(run.ID)
			if !slices.Contains(types(events), "permission_requested") {
				t.Errorf("no permission_requested event in %v", types(events))
			}
			approved := tc.method == ipc.MethodApprove
			wantAnswer := fmt.Sprintf(`{"id":"perm-1","approved":%t}`, approved)
			if got := dataOf(events, "permission_answered"); got != wantAnswer {
				t.Errorf("permission_answered = %s, want %s", got, wantAnswer)
			}

			_, err := h.call(ipc.Request{Method: tc.method, RunID: run.ID})
			if code := codeOf(err); code != ipc.CodeConflict {
				t.Errorf("%s on a finished run: %v, want code %s", tc.method, err, ipc.CodeConflict)
			}
		})
	}
}

func TestStopMidRun(t *testing.T) {
	prompted := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	agent := &fakeAgent{onPrompt: func(_ context.Context, s *fakeSession, _ string) error {
		defer close(returned)
		s.sink.Event(chunk("partial"))
		close(prompted)
		<-release
		s.sink.Event(chunk(" late"))
		return nil
	}}
	h := newHarness(t, agent)

	run := h.launch("work")
	<-prompted
	stopped := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	if stopped.Status != "stopped" {
		t.Fatalf("status = %s, want stopped", stopped.Status)
	}
	if _, err := os.Stat(run.Worktree); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("worktree after stop: %v, want it gone", err)
	}
	waitClosed(t, h.agent.session(0))
	if hook := h.hooks.next(t); hook.Status != "stopped" {
		t.Errorf("webhook = %+v, want stopped", hook)
	}

	close(release)
	<-returned
	again := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	if again.Status != "stopped" || again.Summary != "" {
		t.Errorf("after the late prompt return and a second stop: %+v", again)
	}
	events := h.logs(run.ID)
	if got := statuses(events); !slices.Equal(got, []string{"queued", "running", "stopped"}) {
		t.Errorf("statuses = %v", got)
	}
	for _, e := range events {
		if strings.Contains(string(e.Data), "late") {
			t.Errorf("an event from the closed session was stored: %s", e.Data)
		}
	}
	_, err := h.call(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "more"})
	if code := codeOf(err); code != ipc.CodeConflict {
		t.Errorf("follow-up on a stopped run: %v, want code %s", err, ipc.CodeConflict)
	}
}

func TestStopWhileStartingClosesTheLateSession(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	agent := &fakeAgent{onStart: func() {
		close(entered)
		<-release
	}}
	h := newHarness(t, agent)

	run := h.launch("hello")
	<-entered
	if stopped := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID}); stopped.Status != "stopped" {
		t.Fatalf("status = %s, want stopped", stopped.Status)
	}
	close(release)
	waitClosed(t, h.agent.session(0))

	again := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	if again.Status != "stopped" || again.SessionID != "" {
		t.Errorf("a session that started after stop changed the run: %+v", again)
	}
	if got := h.agent.session(0).sentPrompts(); len(got) != 0 {
		t.Errorf("prompts sent to the late session: %v", got)
	}
}

func TestLateReportsFromAClosedSessionAreDropped(t *testing.T) {
	prompted := make(chan struct{})
	release := make(chan struct{})
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "again" {
			close(prompted)
			<-release
		}
		return echo(ctx, s, prompt)
	}}
	h := newHarness(t, agent)

	run := h.launch("hello")
	h.waitStatus(run.ID, "finished")
	old := h.agent.session(0)
	waitClosed(t, old)
	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "again"})
	<-prompted

	old.sink.Event(chunk("stale"))
	if ok, err := old.sink.Permission(context.Background(), backend.PermissionRequest{Title: "stale"}); ok || err == nil {
		t.Errorf("permission from a closed session = %v, %v; want a refusal", ok, err)
	}
	close(release)

	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "echo: again" {
		t.Errorf("summary = %q, want only the new turn's text", done.Summary)
	}
	for _, e := range h.logs(run.ID) {
		if strings.Contains(string(e.Data), "stale") {
			t.Errorf("an event from the closed session was stored: %s %s", e.Type, e.Data)
		}
	}
}

func TestBackendErrorsFailTheRun(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		h := newHarness(t, &fakeAgent{startErr: errors.New("not logged in")})
		run := h.launch("hello")
		failed := h.waitStatus(run.ID, "failed")
		if !strings.Contains(failed.Error, "not logged in") {
			t.Errorf("error = %q", failed.Error)
		}
		if hook := h.hooks.next(t); hook.Status != "failed" || hook.Error != failed.Error {
			t.Errorf("webhook = %+v", hook)
		}
		waitClosed(t, h.agent.session(0))
	})
	t.Run("prompt", func(t *testing.T) {
		h := newHarness(t, &fakeAgent{onPrompt: func(context.Context, *fakeSession, string) error {
			return errors.New("agent exited with status 3")
		}})
		run := h.launch("hello")
		failed := h.waitStatus(run.ID, "failed")
		if !strings.Contains(failed.Error, "agent exited with status 3") {
			t.Errorf("error = %q", failed.Error)
		}
		waitClosed(t, h.agent.session(0))
		_, err := h.call(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "retry"})
		if code := codeOf(err); code != ipc.CodeConflict {
			t.Errorf("follow-up on a failed run: %v, want code %s", err, ipc.CodeConflict)
		}
	})
	t.Run("worktree", func(t *testing.T) {
		h := newHarness(t, &fakeAgent{})
		run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Ref: "no-such-ref", Prompt: "hello"})
		failed := h.waitStatus(run.ID, "failed")
		if !strings.Contains(failed.Error, "no-such-ref") {
			t.Errorf("error = %q, want git's message about the ref", failed.Error)
		}
	})
}

func TestRecoverFailsRunsThatWereActive(t *testing.T) {
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "work" {
			<-ctx.Done()
			return ctx.Err()
		}
		return echo(ctx, s, prompt)
	}}
	h := newHarness(t, agent)
	finished := h.launch("hello")
	h.waitStatus(finished.ID, "finished")
	h.hooks.next(t)
	active := h.launch("work")
	h.waitStatus(active.ID, "running")
	h.stop()

	waitClosed(t, h.agent.session(1))
	if r, err := h.store.GetRun(context.Background(), store.RunID(active.ID)); err != nil || r.Lifecycle.State != lifecycle.Running {
		t.Fatalf("stored run after shutdown = %s, %v; want it still running", r.Lifecycle.State, err)
	}
	if err := os.WriteFile(h.ndjsonPath(active.ID), []byte(`{"torn`), 0o600); err != nil {
		t.Fatal(err)
	}

	d := h.newDaemon()
	for range 2 {
		if err := d.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h.serve(d)

	failed := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: active.ID})
	if failed.Status != "failed" || failed.Error != "daemon restarted while the run was active" {
		t.Errorf("recovered run = %+v", failed)
	}
	if hook := h.hooks.next(t); hook.RunID != active.ID || hook.Status != "failed" {
		t.Errorf("webhook = %+v, want failed for %s", hook, active.ID)
	}
	if got := statuses(h.waitLog(active.ID, hasType("webhook"))); !slices.Equal(got, []string{"queued", "running", "failed"}) {
		t.Errorf("statuses = %v, want one failure", got)
	}
	if r := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: finished.ID}); r.Status != "finished" {
		t.Errorf("a finished run became %s", r.Status)
	}
	h.waitNDJSONMatchesLogs(active.ID)
	h.waitNDJSONMatchesLogs(finished.ID)
}

func TestLogsFollowEndsWhenTheRunStops(t *testing.T) {
	agent := &fakeAgent{onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	h := newHarness(t, agent)
	run := h.launch("work")
	h.waitStatus(run.ID, "running")

	events := make(chan ipc.Event, 100)
	done := make(chan error, 1)
	go func() {
		req := ipc.Request{Method: ipc.MethodLogs, RunID: run.ID, Follow: true}
		done <- ipc.Call(context.Background(), h.cfg.SocketPath, req, func(r ipc.Response) error {
			events <- *r.Event
			return nil
		})
	}()
	<-events
	h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(timeout):
		t.Fatal("logs --follow still running after the run stopped")
	}
	close(events)
	var last string
	for e := range events {
		if e.Type == "status" {
			last = string(e.Data)
		}
	}
	if last != `{"status":"stopped"}` {
		t.Errorf("last status event = %s, want stopped", last)
	}
}

func TestCallErrors(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	notRepo := t.TempDir()
	for _, tc := range []struct {
		name string
		req  ipc.Request
		code string
	}{
		{"status of an unknown run", ipc.Request{Method: ipc.MethodStatus, RunID: "run-000000000000"}, ipc.CodeNotFound},
		{"stop of an unknown run", ipc.Request{Method: ipc.MethodStop, RunID: "run-000000000000"}, ipc.CodeNotFound},
		{"logs of an unknown run", ipc.Request{Method: ipc.MethodLogs, RunID: "run-000000000000"}, ipc.CodeNotFound},
		{"launch without a prompt", ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: " "}, ipc.CodeInvalid},
		{"launch outside a repository", ipc.Request{Method: ipc.MethodLaunch, Repo: notRepo, Prompt: "hi"}, ipc.CodeInvalid},
		{"launch with a relative repo", ipc.Request{Method: ipc.MethodLaunch, Repo: "repo", Prompt: "hi"}, ipc.CodeInvalid},
		{"launch with an unknown backend", ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Backend: "web"}, ipc.CodeInvalid},
		{"launch with a backend the daemon lacks", ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hi", Backend: "print"}, ipc.CodeInvalid},
		{"follow-up without a prompt", ipc.Request{Method: ipc.MethodFollowUp, RunID: "run-000000000000"}, ipc.CodeInvalid},
		{"unknown method", ipc.Request{Method: "restart"}, ipc.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ipc.Call(context.Background(), h.cfg.SocketPath, tc.req, func(ipc.Response) error { return nil })
			if code := codeOf(err); code != tc.code {
				t.Errorf("err = %v, want code %s", err, tc.code)
			}
		})
	}
	if runs := h.list(); len(runs) != 0 {
		t.Errorf("failed launches created runs: %+v", runs)
	}
}

func TestListShowsEveryRunNewestFirst(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	first := h.launch("one")
	h.waitStatus(first.ID, "finished")
	second := h.launch("two")
	h.waitStatus(second.ID, "finished")

	runs := h.list()
	if len(runs) != 2 || runs[0].ID != second.ID || runs[1].ID != first.ID {
		t.Fatalf("list = %+v", runs)
	}
}

// harness runs a daemon with a fake agent, the real store, a real git
// repository, and a webhook receiver that checks signatures.
type harness struct {
	t     *testing.T
	cfg   config.Config
	repo  string
	store *store.Store
	agent *fakeAgent
	hooks *hookServer
	stop  func()
}

func newHarness(t *testing.T, agent *fakeAgent) *harness {
	t.Helper()
	// macOS limits unix socket paths to 104 bytes and t.TempDir is long there.
	home, err := os.MkdirTemp("", "clb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, cfg: cfg, repo: gitRepo(t), store: st, agent: agent, hooks: newHookServer(t)}
	h.serve(h.newDaemon())
	return h
}

func (h *harness) newDaemon() *daemon.Daemon {
	return daemon.New(daemon.Options{
		Store:    h.store,
		Backends: daemon.Backends{backend.ACP: h.agent.newSession},
		Config:   h.cfg,
		Webhook:  h.hooks.sender(),
		Logger:   slog.New(slog.NewTextHandler(h.t.Output(), nil)),
	})
}

func (h *harness) serve(d *daemon.Daemon) {
	h.t.Helper()
	ln, err := ipc.Listen(h.cfg.SocketPath)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, ln) }()
	var once sync.Once
	h.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				h.t.Errorf("Serve: %v", err)
			}
		})
	}
	h.t.Cleanup(h.stop)
}

func (h *harness) call(req ipc.Request) (ipc.Run, error) {
	var run ipc.Run
	err := ipc.Call(context.Background(), h.cfg.SocketPath, req, func(r ipc.Response) error {
		if r.Run != nil {
			run = *r.Run
		}
		return nil
	})
	return run, err
}

func (h *harness) must(req ipc.Request) ipc.Run {
	h.t.Helper()
	run, err := h.call(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.RunID, err)
	}
	return run
}

func (h *harness) launch(prompt string) ipc.Run {
	h.t.Helper()
	return h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: prompt})
}

func (h *harness) list() []ipc.Run {
	h.t.Helper()
	var runs []ipc.Run
	err := ipc.Call(context.Background(), h.cfg.SocketPath, ipc.Request{Method: ipc.MethodList}, func(r ipc.Response) error {
		runs = append(runs, r.Runs...)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return runs
}

func (h *harness) waitStatus(id, want string) ipc.Run {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		run := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: id})
		if run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run %s is %s, want %s: %+v", id, run.Status, want, run)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) logs(id string) []ipc.Event {
	h.t.Helper()
	var events []ipc.Event
	err := ipc.Call(context.Background(), h.cfg.SocketPath, ipc.Request{Method: ipc.MethodLogs, RunID: id}, func(r ipc.Response) error {
		events = append(events, *r.Event)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return events
}

func (h *harness) waitLog(id string, ok func([]ipc.Event) bool) []ipc.Event {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		events := h.logs(id)
		if ok(events) {
			return events
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run %s log never matched: %v", id, types(events))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) ndjsonPath(id string) string {
	return filepath.Join(h.cfg.LogDir(), id+".ndjson")
}

// waitNDJSONMatchesLogs waits because the NDJSON append follows the store
// insert that the logs call can already see.
func (h *harness) waitNDJSONMatchesLogs(id string) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var want strings.Builder
		for _, e := range h.logs(id) {
			line, err := json.Marshal(e)
			if err != nil {
				h.t.Fatal(err)
			}
			want.Write(line)
			want.WriteByte('\n')
		}
		got, err := os.ReadFile(h.ndjsonPath(id))
		if err == nil && string(got) == want.String() {
			if fi, err := os.Stat(h.ndjsonPath(id)); err != nil || fi.Mode().Perm() != 0o600 {
				h.t.Errorf("NDJSON log mode: %v, %v", fi.Mode(), err)
			}
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("NDJSON log of %s:\n%s\nwant the logs call's events:\n%s", id, got, want.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeAgent is an in-memory backend. Each Start returns session-<n>, or the
// resumed ID. Prompts reply "echo: <prompt>" unless onPrompt is set.
type fakeAgent struct {
	startErr error
	onStart  func()
	onPrompt func(ctx context.Context, s *fakeSession, prompt string) error

	mu       sync.Mutex
	sessions []*fakeSession
}

func (f *fakeAgent) newSession(sink backend.Sink) daemon.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSession{agent: f, sink: sink, n: len(f.sessions) + 1, closed: make(chan struct{})}
	f.sessions = append(f.sessions, s)
	return s
}

func (f *fakeAgent) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func (f *fakeAgent) session(i int) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[i]
}

type fakeSession struct {
	agent     *fakeAgent
	sink      backend.Sink
	n         int
	closed    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	opts    backend.StartOptions
	prompts []string
}

func (s *fakeSession) Start(_ context.Context, opts backend.StartOptions) (string, error) {
	s.mu.Lock()
	s.opts = opts
	s.mu.Unlock()
	if s.agent.onStart != nil {
		s.agent.onStart()
	}
	if s.agent.startErr != nil {
		return "", s.agent.startErr
	}
	if opts.ResumeSessionID != "" {
		return opts.ResumeSessionID, nil
	}
	return fmt.Sprintf("session-%d", s.n), nil
}

func (s *fakeSession) Prompt(ctx context.Context, text string) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, text)
	s.mu.Unlock()
	if s.agent.onPrompt != nil {
		return s.agent.onPrompt(ctx, s, text)
	}
	return echo(ctx, s, text)
}

func (s *fakeSession) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeSession) startOptions() backend.StartOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

func (s *fakeSession) sentPrompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.prompts)
}

func waitClosed(t *testing.T, s *fakeSession) {
	t.Helper()
	select {
	case <-s.closed:
	case <-time.After(timeout):
		t.Fatalf("session-%d was never closed", s.n)
	}
}

func echo(_ context.Context, s *fakeSession, prompt string) error {
	s.sink.Event(chunk("echo: "))
	s.sink.Event(backend.Event{Type: "tool_call", Data: json.RawMessage(`{"sessionUpdate":"tool_call","title":"Read file"}`)})
	s.sink.Event(chunk(prompt))
	return nil
}

func askPermission(ctx context.Context, s *fakeSession, _ string) error {
	approved, err := s.sink.Permission(ctx, backend.PermissionRequest{
		Title:    "Run fake command",
		ToolCall: json.RawMessage(`{"toolCallId":"call-1"}`),
	})
	if err != nil {
		return err
	}
	answer := "denied"
	if approved {
		answer = "approved"
	}
	s.sink.Event(chunk("permission: " + answer))
	return nil
}

func chunk(text string) backend.Event {
	data, _ := json.Marshal(map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]string{"type": "text", "text": text},
	})
	return backend.Event{Type: "agent_message_chunk", Data: data, Text: text}
}

type hookServer struct {
	srv      *httptest.Server
	secret   []byte
	payloads chan webhook.Payload
}

func newHookServer(t *testing.T) *hookServer {
	h := &hookServer{secret: []byte("test secret"), payloads: make(chan webhook.Payload, 100)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !webhook.Verify(h.secret, body, r.Header.Get(webhook.SignatureHeader)) {
			t.Errorf("webhook signature does not verify: %s", body)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var p webhook.Payload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("webhook body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.payloads <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookServer) sender() *webhook.Sender {
	return &webhook.Sender{
		URL:      h.srv.URL,
		Secret:   h.secret,
		Client:   h.srv.Client(),
		Backoff:  10 * time.Millisecond,
		Attempts: 3,
	}
}

func (h *hookServer) next(t *testing.T) webhook.Payload {
	t.Helper()
	select {
	case p := <-h.payloads:
		return p
	case <-time.After(timeout):
		t.Fatal("no webhook arrived")
		return webhook.Payload{}
	}
}

// gitRepo returns a repository with one commit, by its resolved path, which
// is how git reports it.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "README.md"},
		{"-c", "user.name=clb", "-c", "user.email=clb@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func types(events []ipc.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func statuses(events []ipc.Event) []string {
	var out []string
	for _, e := range events {
		if e.Type != "status" {
			continue
		}
		var s struct{ Status string }
		_ = json.Unmarshal(e.Data, &s)
		out = append(out, s.Status)
	}
	return out
}

func dataOf(events []ipc.Event, typ string) string {
	for _, e := range events {
		if e.Type == typ {
			return string(e.Data)
		}
	}
	return ""
}

func hasType(typ string) func([]ipc.Event) bool {
	return func(events []ipc.Event) bool { return slices.Contains(types(events), typ) }
}

func codeOf(err error) string {
	var e *ipc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fmt.Sprintf("no ipc error (%v)", err)
}
