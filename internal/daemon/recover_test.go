package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
)

func TestRecoverEndsProcessesLeftInAnActiveRunsWorktree(t *testing.T) {
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	run := h.launch("work")
	h.waitStatus(run.ID, "running")
	// Serve returns after the session's sweep of the worktree, so the sleeps
	// started after it stand for commands a crashed daemon's agent left.
	h.stop()
	inWorktree, inRepo := sleepIn(t, run.Worktree), sleepIn(t, h.repo)
	if r, err := h.store.GetRun(context.Background(), store.RunID(run.ID)); err != nil || r.Lifecycle.State != lifecycle.Running {
		t.Fatalf("stored run after the crash = %s, %v; want it still running", r.Lifecycle.State, err)
	}

	d := h.newDaemon()
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.serve(d)

	select {
	case <-inWorktree:
	case <-time.After(timeout):
		t.Fatalf("a sleep working in the worktree still runs %v after Recover", timeout)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case <-inRepo:
		t.Error("Recover ended a sleep working in the source repository")
	default:
	}
}
