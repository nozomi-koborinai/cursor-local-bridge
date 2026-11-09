package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

func TestStallWatchdogFailsSilentRunAndResumesOnSameWorktree(t *testing.T) {
	const stallLimit = 80 * time.Millisecond
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "hang" {
			if err := os.WriteFile(filepath.Join(s.startOptions().Dir, "partial.txt"), []byte("saved-in-worktree\n"), 0o600); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}
		return echo(ctx, s, prompt)
	}}
	h := newHarness(t, agent, func(cfg *config.Config) {
		cfg.StallTimeout = config.Duration(stallLimit)
	})

	run := h.launch("hang")
	failed := h.waitStatus(run.ID, "failed")
	wantErr := "run stalled: no agent activity for " + stallLimit.String()
	if failed.Error != wantErr {
		t.Fatalf("error = %q, want %q", failed.Error, wantErr)
	}
	waitClosed(t, h.agent.session(0))

	hook := h.hooks.next(t)
	if hook.RunID != run.ID || hook.Status != "failed" || hook.Error != wantErr {
		t.Fatalf("webhook = %+v, want failed with %q", hook, wantErr)
	}

	resumed := h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "continue"})
	if resumed.Status != "queued" || resumed.Worktree != run.Worktree || resumed.Branch != run.Branch {
		t.Fatalf("resumed = %+v", resumed)
	}
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "continue" || done.Worktree != run.Worktree {
		t.Fatalf("done = %+v", done)
	}
	if got := h.agent.session(1).startOptions().ResumeSessionID; got != failed.SessionID || got == "" {
		t.Fatalf("resumed session ID = %q, want %q", got, failed.SessionID)
	}
	body, err := os.ReadFile(filepath.Join(done.Worktree, "partial.txt"))
	if err != nil || string(body) != "saved-in-worktree\n" {
		t.Fatalf("partial.txt in worktree = %q, %v", body, err)
	}
	if hook := h.hooks.next(t); hook.Status != "finished" || hook.Summary != "continue" {
		t.Fatalf("second webhook = %+v", hook)
	}
}

func TestStallWatchdogFailsQueuedRunStuckInStartAndResumesFreshSession(t *testing.T) {
	const stallLimit = 80 * time.Millisecond
	agent := &fakeAgent{onStart: func(ctx context.Context, s *fakeSession) {
		if s.n == 1 {
			<-ctx.Done()
		}
	}}
	h := newHarness(t, agent, func(cfg *config.Config) {
		cfg.StallTimeout = config.Duration(stallLimit)
	})

	run := h.launch("hello")
	failed := h.waitStatus(run.ID, "failed")
	wantErr := "run stalled: no agent activity for " + stallLimit.String()
	if failed.Error != wantErr || failed.SessionID != "" {
		t.Fatalf("failed = %+v, want error %q and empty sessionId", failed, wantErr)
	}
	if hook := h.hooks.next(t); hook.Status != "failed" || hook.Error != wantErr {
		t.Fatalf("webhook = %+v", hook)
	}
	waitClosed(t, h.agent.session(0))

	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "retry after start failure"})
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "retry after start failure" || done.SessionID != "session-2" {
		t.Fatalf("done = %+v", done)
	}
	if got := h.agent.session(1).startOptions().ResumeSessionID; got != "" {
		t.Fatalf("ResumeSessionID = %q, want empty when first start never produced a session", got)
	}
}

func TestStallWatchdogResetsOnUnstoredActivityAndPausesForPermission(t *testing.T) {
	const stallLimit = 100 * time.Millisecond
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		for range 3 {
			time.Sleep(40 * time.Millisecond)
			s.sink.Event(backend.Event{
				Type: "agent_thought_chunk",
				Data: json.RawMessage(`{"sessionUpdate":"agent_thought_chunk"}`),
			})
		}
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:    "Run command",
			ToolCall: json.RawMessage(`{"toolCallId":"call-1"}`),
		})
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("denied")
		}
		s.sink.Event(chunk("completed after permission"))
		return nil
	}}
	h := newHarness(t, agent, func(cfg *config.Config) {
		cfg.StallTimeout = config.Duration(stallLimit)
	})

	run := h.launch("work")
	h.waitStatus(run.ID, "awaiting_permission")
	h.agent.session(0).sink.Event(chunk("still waiting on user"))
	time.Sleep(2 * stallLimit)
	stillWaiting := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: run.ID})
	if stillWaiting.Status != "awaiting_permission" {
		t.Fatalf("status while awaiting permission = %s, want awaiting_permission", stillWaiting.Status)
	}

	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "still waiting on usercompleted after permission" {
		t.Fatalf("summary = %q", done.Summary)
	}
}

func TestStallWatchdogIsNotExtendedByQueuedFollowUps(t *testing.T) {
	const stallLimit = 120 * time.Millisecond
	agent := &fakeAgent{onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	h := newHarness(t, agent, func(cfg *config.Config) {
		cfg.StallTimeout = config.Duration(stallLimit)
	})

	run := h.launch("hang")
	h.waitStatus(run.ID, "running")
	began := time.Now()
	for range 3 {
		time.Sleep(stallLimit / 2)
		cur := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: run.ID})
		if cur.Status == "failed" {
			break
		}
		_, _ = h.call(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "queued"})
	}
	failed := h.waitStatus(run.ID, "failed")
	if took := time.Since(began); took > 3*stallLimit {
		t.Fatalf("run took %v to stall despite no agent events; follow-ups extended the timer", took)
	}
	wantErr := "run stalled: no agent activity for " + stallLimit.String()
	if failed.Error != wantErr {
		t.Fatalf("error = %q, want %q", failed.Error, wantErr)
	}
}

func TestKeepAwakeHoldsAssertionWhileAnyRunIsActive(t *testing.T) {
	holdFirst := make(chan struct{})
	holdSecond := make(chan struct{})
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		switch prompt {
		case "first":
			<-holdFirst
		case "second":
			<-holdSecond
		}
		return echo(ctx, s, prompt)
	}}

	started := make(chan struct{}, 10)
	stopped := make(chan struct{}, 10)
	h := newHarness(t, agent, func(cfg *config.Config) {
		cfg.KeepAwake = true
	})
	h.stop()
	h.keepAwake = func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return nil
	}
	h.serve(h.newDaemon())

	select {
	case <-started:
		t.Fatal("keep-awake started before any run was launched")
	default:
	}

	r1 := h.launch("first")
	select {
	case <-started:
	case <-time.After(timeout):
		t.Fatal("keep-awake did not start when first run launched")
	}

	r2 := h.launch("second")
	h.waitStatus(r1.ID, "running")
	h.waitStatus(r2.ID, "running")
	select {
	case <-started:
		t.Fatal("keep-awake started a second time while already held")
	default:
	}

	close(holdFirst)
	h.waitStatus(r1.ID, "finished")
	select {
	case <-stopped:
		t.Fatal("keep-awake stopped while second run was still active")
	case <-time.After(50 * time.Millisecond):
	}

	close(holdSecond)
	h.waitStatus(r2.ID, "finished")
	select {
	case <-stopped:
	case <-time.After(timeout):
		t.Fatal("keep-awake did not stop after all runs finished")
	}

	r3 := h.launch("third")
	select {
	case <-started:
	case <-time.After(timeout):
		t.Fatal("keep-awake did not restart for a new run")
	}
	h.waitStatus(r3.ID, "finished")
	select {
	case <-stopped:
	case <-time.After(timeout):
		t.Fatal("keep-awake did not stop after third run finished")
	}
}

func TestKeepAwakeRestartsAfterEarlyExit(t *testing.T) {
	hold := make(chan struct{})
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "stay-active" {
			<-hold
		}
		return echo(ctx, s, prompt)
	}}

	started := make(chan int, 10)
	var calls int
	h := newHarness(t, agent, func(cfg *config.Config) {
		cfg.KeepAwake = true
	})
	h.stop()
	h.keepAwake = func(ctx context.Context) error {
		calls++
		n := calls
		started <- n
		if n == 1 {
			return errors.New("caffeinate exited early")
		}
		<-ctx.Done()
		return nil
	}
	h.serve(h.newDaemon())

	r1 := h.launch("stay-active")
	select {
	case n := <-started:
		if n != 1 {
			t.Fatalf("first call = %d, want 1", n)
		}
	case <-time.After(timeout):
		t.Fatal("keep-awake did not start on first run")
	}
	h.waitStatus(r1.ID, "running")
	r2 := h.launch("quick")
	h.waitStatus(r2.ID, "finished")
	select {
	case n := <-started:
		if n != 2 {
			t.Fatalf("second call = %d, want 2", n)
		}
	case <-time.After(timeout):
		t.Fatal("keep-awake did not restart after early exit while runs were active")
	}
	close(hold)
	h.waitStatus(r1.ID, "finished")
}

func TestFollowUpOnStoppedRunStillConflicts(t *testing.T) {
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "fail" {
			return errors.New("boom")
		}
		return echo(ctx, s, prompt)
	}})
	run := h.launch("hello")
	h.waitStatus(run.ID, "finished")
	h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})

	_, err := h.call(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "after stop"})
	if code := codeOf(err); code != ipc.CodeConflict || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("follow-up on stopped run = %v, want conflict", err)
	}

	failedRun := h.launch("fail")
	h.waitStatus(failedRun.ID, "failed")
	stoppedFailed := h.must(ipc.Request{Method: ipc.MethodStop, RunID: failedRun.ID})
	if stoppedFailed.Status != "failed" {
		t.Fatalf("stop of failed run status = %s, want failed", stoppedFailed.Status)
	}
	_, err = h.call(ipc.Request{Method: ipc.MethodFollowUp, RunID: failedRun.ID, Prompt: "after stop of failed"})
	if code := codeOf(err); code != ipc.CodeConflict || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("follow-up on stopped failed run = %v, want conflict", err)
	}
	if _, statErr := os.Stat(failedRun.Worktree); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree of stopped failed run = %v, want it absent", statErr)
	}
}
