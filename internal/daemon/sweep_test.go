package daemon_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/daemon"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

func TestALateSweepSparesTheSessionThatReplacedIt(t *testing.T) {
	daemon.SetCloseWait(t, 50*time.Millisecond)
	closeFirst := make(chan struct{})
	finishSecond := make(chan struct{})
	agent := &fakeAgent{
		onClose: func(s *fakeSession) {
			if s.n == 1 {
				<-closeFirst
			}
		},
		onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
			if s.n == 2 {
				select {
				case <-finishSecond:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return echo(ctx, s, prompt)
		},
	}
	h := newHarness(t, agent)
	releaseFirst := sync.OnceFunc(func() { close(closeFirst) })
	t.Cleanup(releaseFirst)

	run := h.launch("hello")
	h.waitStatus(run.ID, "finished")
	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "again"})
	h.waitStatus(run.ID, "running")
	inWorktree := sleepIn(t, run.Worktree)

	releaseFirst()
	waitClosed(t, h.agent.session(0))
	select {
	case <-inWorktree:
		t.Fatal("the first session's late sweep ended a process working for the session that replaced it")
	case <-time.After(time.Second):
	}

	close(finishSecond)
	h.waitStatus(run.ID, "finished")
	select {
	case <-inWorktree:
	case <-time.After(timeout):
		t.Fatalf("a sleep working in the worktree still runs %v after the second session finished", timeout)
	}
}
