package daemon_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

func TestCleanDryRunThenRemovesFinishedWorktreesAndBranches(t *testing.T) {
	release := make(chan struct{})
	agent := &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		switch prompt {
		case "block":
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return echo(ctx, s, prompt)
		case "permit":
			return askPermission(ctx, s, prompt)
		case "fail":
			return errors.New("boom")
		default:
			return echo(ctx, s, prompt)
		}
	}}
	h := newHarness(t, agent)
	plain := resolvedTemp(t)
	if err := os.WriteFile(plain+"/keep.txt", []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	finished := h.launch("ok")
	finished = h.waitStatus(finished.ID, "finished")
	if err := os.WriteFile(finished.Worktree+"/dirty.txt", []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := h.launch("fail")
	failed = h.waitStatus(failed.ID, "failed")
	stopped := h.launch("ok")
	h.waitStatus(stopped.ID, "finished")
	stopped = h.must(ipc.Request{Method: ipc.MethodStop, RunID: stopped.ID})
	plainRun := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: plain, Prompt: "ok", NoWorktree: true})
	plainRun = h.waitStatus(plainRun.ID, "finished")
	running := h.launch("block")
	running = h.waitStatus(running.ID, "running")
	waiting := h.launch("permit")
	waiting = h.waitStatus(waiting.ID, "awaiting_permission")

	// Dry-run waits out the finished sessions' sweeps before the sleeps below
	// are started, so a later EndUnder is clean's and not the finish sweep.
	dry := h.clean(true)
	want := []ipc.CleanedRun{
		{ID: stopped.ID, Status: "stopped", Repo: h.repo, Branch: stopped.Branch},
		{ID: failed.ID, Status: "failed", Repo: h.repo, Branch: failed.Branch, Worktree: failed.Worktree},
		{ID: finished.ID, Status: "finished", Repo: h.repo, Branch: finished.Branch, Worktree: finished.Worktree},
	}
	if !dry.DryRun || !sameCleaned(dry.Runs, want) {
		t.Fatalf("dry-run = %+v, want %+v", dry.Runs, want)
	}
	assertKept(t, finished.Worktree, failed.Worktree, running.Worktree, waiting.Worktree, plain)
	for _, branch := range []string{finished.Branch, failed.Branch, stopped.Branch, running.Branch, waiting.Branch} {
		if !strings.Contains(clbBranches(t, h.repo), branch) {
			t.Fatalf("dry-run deleted %s\n%s", branch, clbBranches(t, h.repo))
		}
	}
	if body, err := os.ReadFile(finished.Worktree + "/dirty.txt"); err != nil || string(body) != "dirty\n" {
		t.Fatalf("dirty file after dry-run = %q, %v", body, err)
	}

	inFinished, inFailed := sleepIn(t, finished.Worktree), sleepIn(t, failed.Worktree)
	inRunning, inWaiting := sleepIn(t, running.Worktree), sleepIn(t, waiting.Worktree)
	inPlain := sleepIn(t, plain)
	again := h.clean(true)
	if !again.DryRun || !sameCleaned(again.Runs, want) {
		t.Fatalf("second dry-run = %+v, want the same report", again.Runs)
	}
	assertStillRunning(t, inFinished, inFailed, inRunning, inWaiting, inPlain)

	cleaned := h.clean(false)
	if cleaned.DryRun || !sameCleaned(cleaned.Runs, want) {
		t.Fatalf("clean = %+v, want %+v", cleaned, want)
	}
	assertGone(t, finished.Worktree, failed.Worktree)
	assertKept(t, running.Worktree, waiting.Worktree, plain)
	if body, err := os.ReadFile(plain + "/keep.txt"); err != nil || string(body) != "keep\n" {
		t.Fatalf("plain file after clean = %q, %v", body, err)
	}
	branches := clbBranches(t, h.repo)
	for _, branch := range []string{finished.Branch, failed.Branch, stopped.Branch} {
		if strings.Contains(branches, branch) {
			t.Fatalf("clean left %s\n%s", branch, branches)
		}
	}
	for _, branch := range []string{running.Branch, waiting.Branch} {
		if !strings.Contains(branches, branch) {
			t.Fatalf("clean deleted active branch %s\n%s", branch, branches)
		}
	}
	assertExited(t, inFinished, inFailed)
	assertStillRunning(t, inRunning, inWaiting, inPlain)

	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: finished.ID}); got.Status != "finished" {
		t.Fatalf("finished status = %s", got.Status)
	}
	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: failed.ID}); got.Status != "failed" {
		t.Fatalf("failed status = %s", got.Status)
	}
	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: stopped.ID}); got.Status != "stopped" {
		t.Fatalf("stopped status = %s", got.Status)
	}
	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: running.ID}); got.Status != "running" {
		t.Fatalf("running status = %s", got.Status)
	}
	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: waiting.ID}); got.Status != "awaiting_permission" {
		t.Fatalf("waiting status = %s", got.Status)
	}
	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: plainRun.ID}); got.Status != "finished" || got.Worktree != "" {
		t.Fatalf("plain run = %+v", got)
	}

	second := h.clean(false)
	if second.DryRun || second.Runs == nil || len(second.Runs) != 0 {
		t.Fatalf("second clean = %+v, want no runs", second)
	}
	close(release)
}

func TestCleanAndStopLeaveAConsistentCheckout(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	run := h.launch("hello")
	h.waitStatus(run.ID, "finished")
	if err := os.WriteFile(run.Worktree+"/note.txt", []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := h.call(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := h.call(ipc.Request{Method: ipc.MethodClean})
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent call: %v", err)
		}
	}
	if _, err := os.Stat(run.Worktree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree = %v, want it gone", err)
	}
	if strings.Contains(clbBranches(t, h.repo), run.Branch) {
		t.Errorf("branch %s still exists\n%s", run.Branch, clbBranches(t, h.repo))
	}
	if got := h.must(ipc.Request{Method: ipc.MethodStatus, RunID: run.ID}); got.Status != "stopped" {
		t.Fatalf("status = %s, want stopped", got.Status)
	}
}

func (h *harness) clean(dryRun bool) ipc.CleanResult {
	h.t.Helper()
	var result ipc.CleanResult
	var saw bool
	err := ipc.Call(context.Background(), h.cfg.SocketPath, ipc.Request{Method: ipc.MethodClean, DryRun: dryRun}, func(r ipc.Response) error {
		if r.Clean != nil {
			result = *r.Clean
			saw = true
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if !saw {
		h.t.Fatal("clean reply had no result")
	}
	return result
}

func sameCleaned(got, want []ipc.CleanedRun) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func assertKept(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("stat %s: %v, want it kept", path, err)
		}
	}
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stat %s: %v, want it gone", path, err)
		}
	}
}

func assertStillRunning(t *testing.T, sleeps ...<-chan struct{}) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	for i, ch := range sleeps {
		select {
		case <-ch:
			t.Errorf("sleep %d exited", i)
		default:
		}
	}
}

func assertExited(t *testing.T, sleeps ...<-chan struct{}) {
	t.Helper()
	for i, ch := range sleeps {
		select {
		case <-ch:
		case <-time.After(timeout):
			t.Errorf("sleep %d still runs after %v", i, timeout)
		}
	}
}
