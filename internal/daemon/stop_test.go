package daemon_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
)

func TestAStopWhoseRemovalFailsFailsTheRun(t *testing.T) {
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}})
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

	_, stopErr := h.call(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	var ie *ipc.Error
	if !errors.As(stopErr, &ie) || ie.Code != ipc.CodeInternal || !strings.HasPrefix(ie.Message, "remove worktree: ") {
		t.Fatalf("stop = %v, want an internal error about removing the worktree", stopErr)
	}
	failed := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: run.ID})
	if failed.Status != "failed" || failed.Error != ie.Message {
		t.Errorf("after the failed removal: status %s, error %q; want failed with %q", failed.Status, failed.Error, ie.Message)
	}
	if hook := h.hooks.next(t); hook.Status != "failed" || hook.Error != ie.Message {
		t.Errorf("webhook = %+v, want failed with the removal error", hook)
	}
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Errorf("worktree after the failed removal: %v, want it kept", err)
	}

	if err := os.WriteFile(gitFile, link, 0o600); err != nil {
		t.Fatal(err)
	}
	again := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	if again.Status != "failed" || again.Error != ie.Message {
		t.Errorf("after a retried stop: status %s, error %q; want the failure kept", again.Status, again.Error)
	}
	if _, err := os.Stat(run.Worktree); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("worktree after a retried stop: %v, want it gone", err)
	}
	want := []string{"queued", "running", "failed"}
	if got := statuses(h.waitLog(run.ID, hasType("webhook"))); !slices.Equal(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}

	h.stop()
	select {
	case hook := <-h.hooks.payloads:
		t.Errorf("another webhook arrived: %+v", hook)
	default:
	}
}

func TestAStopDuringShutdownStillRemovesTheWorktree(t *testing.T) {
	closing := make(chan struct{})
	shuttingDown := make(chan struct{})
	finishClose := make(chan struct{})
	agent := &fakeAgent{
		onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
			<-ctx.Done()
			if s.n == 2 {
				close(shuttingDown)
			}
			return ctx.Err()
		},
		onClose: func(s *fakeSession) {
			if s.n == 1 {
				close(closing)
				<-finishClose
			}
		},
	}
	h := newHarness(t, agent)
	releaseClose := sync.OnceFunc(func() { close(finishClose) })
	t.Cleanup(releaseClose)
	stopped := h.launch("stopped during shutdown")
	h.waitStatus(stopped.ID, "running")
	// The second run's agent ends only when the daemon's context does.
	sentinel := h.launch("ends at shutdown")
	h.waitStatus(sentinel.ID, "running")

	go func() { _, _ = h.call(ipc.Request{Method: ipc.MethodStop, RunID: stopped.ID}) }()
	select {
	case <-closing:
	case <-time.After(timeout):
		t.Fatal("stop never closed the agent")
	}
	served := make(chan struct{})
	go func() {
		h.stop()
		close(served)
	}()
	select {
	case <-shuttingDown:
	case <-time.After(timeout):
		t.Fatal("the daemon never began shutting down")
	}
	releaseClose()
	<-served

	if _, err := os.Stat(stopped.Worktree); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("worktree after a stop during shutdown: %v, want it gone", err)
	}
	r, err := h.store.GetRun(context.Background(), store.RunID(stopped.ID))
	if err != nil || string(r.Lifecycle.State) != "stopped" {
		t.Errorf("stored run after a stop during shutdown = %s, %v; want stopped", r.Lifecycle.State, err)
	}
}
