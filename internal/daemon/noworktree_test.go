package daemon_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
)

func TestNoWorktreeRunsInTheDirectory(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	dir := resolvedTemp(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: link, Prompt: "hello", NoWorktree: true})
	if run.Repo != dir || run.BaseRef != "" || run.Branch != "" || run.Worktree != "" {
		t.Fatalf("launched = %+v, want repo %s and no checkout", run, dir)
	}
	done := h.waitStatus(run.ID, "finished")
	if done.Repo != dir || done.BaseRef != "" || done.Branch != "" || done.Worktree != "" {
		t.Fatalf("finished = %+v, want repo %s and no checkout", done, dir)
	}
	if opts := h.agent.session(0).startOptions(); opts.Dir != dir {
		t.Fatalf("start dir = %q, want %s", opts.Dir, dir)
	}
	if body, err := os.ReadFile(note); err != nil || string(body) != "keep\n" {
		t.Fatalf("note after finish = %q, %v", body, err)
	}
	if hook := h.hooks.next(t); hook.Branch != "" || hook.Repo != dir {
		t.Fatalf("webhook = %+v, want repo %s and no branch", hook, dir)
	}

	resumed := h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: run.ID, Prompt: "again"})
	if resumed.Status != "queued" || resumed.Repo != dir || resumed.Worktree != "" {
		t.Fatalf("followup = %+v", resumed)
	}
	done = h.waitStatus(run.ID, "finished")
	if done.Summary != "again" || done.SessionID == "" {
		t.Fatalf("after followup = %+v", done)
	}
	if opts := h.agent.session(1).startOptions(); opts.Dir != dir || opts.ResumeSessionID == "" {
		t.Fatalf("resume options = %+v, want dir %s", opts, dir)
	}
	if body, err := os.ReadFile(note); err != nil || string(body) != "keep\n" {
		t.Fatalf("note after followup = %q, %v", body, err)
	}
}

func TestNoWorktreeStopLeavesTheDirectoryAndDoesNotCommit(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	note := filepath.Join(h.repo, "note.txt")
	run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: h.repo, Prompt: "hello", NoWorktree: true})
	h.waitStatus(run.ID, "finished")
	if branches := clbBranches(t, h.repo); branches != "" {
		t.Fatalf("launch created branches:\n%s", branches)
	}
	before := gitHEAD(t, h.repo)
	if err := os.WriteFile(note, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stopped := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	if stopped.Status != "stopped" || stopped.Worktree != "" || stopped.Branch != "" {
		t.Fatalf("stop = %+v, want stopped with no checkout", stopped)
	}
	if body, err := os.ReadFile(note); err != nil || string(body) != "keep\n" {
		t.Fatalf("note after stop = %q, %v", body, err)
	}
	if got := gitHEAD(t, h.repo); got != before {
		t.Fatalf("HEAD = %s, want %s. stop committed in the repository", got, before)
	}
	if status := gitOutput(t, h.repo, "status", "--porcelain"); !strings.Contains(status, "note.txt") {
		t.Fatalf("status = %q, want note.txt still untracked", status)
	}
	if branches := clbBranches(t, h.repo); branches != "" {
		t.Fatalf("stop created branches:\n%s", branches)
	}
}

func TestNoWorktreeFinishAndStopLeaveProcesses(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return echo(ctx, s, prompt)
	}})
	dir := resolvedTemp(t)
	run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: dir, Prompt: "hello", NoWorktree: true})
	h.waitStatus(run.ID, "running")
	during := sleepIn(t, dir)
	close(release)
	h.waitStatus(run.ID, "finished")
	time.Sleep(200 * time.Millisecond)
	select {
	case <-during:
		t.Fatal("finishing the run ended a sleep in the directory")
	default:
	}

	after := sleepIn(t, dir)
	if stopped := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID}); stopped.Status != "stopped" {
		t.Fatalf("stop status = %s", stopped.Status)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case <-after:
		t.Fatal("stop ended a sleep in the directory")
	default:
	}
	select {
	case <-during:
		t.Fatal("stop ended the sleep that survived finish")
	default:
	}
}

func TestNoWorktreeRecoverLeavesProcesses(t *testing.T) {
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, _ *fakeSession, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	dir := resolvedTemp(t)
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := h.must(ipc.Request{Method: ipc.MethodLaunch, Repo: dir, Prompt: "work", NoWorktree: true})
	h.waitStatus(run.ID, "running")
	h.stop()
	sleeping := sleepIn(t, dir)
	if r, err := h.store.GetRun(context.Background(), store.RunID(run.ID)); err != nil || r.Lifecycle.State != lifecycle.Running {
		t.Fatalf("stored run after the crash = %s, %v; want it still running", r.Lifecycle.State, err)
	}

	d := h.newDaemon()
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.serve(d)

	time.Sleep(200 * time.Millisecond)
	select {
	case <-sleeping:
		t.Fatal("Recover ended a sleep in the directory")
	default:
	}
	if body, err := os.ReadFile(note); err != nil || string(body) != "keep\n" {
		t.Fatalf("note after Recover = %q, %v", body, err)
	}
	failed := h.waitStatus(run.ID, "failed")
	if failed.Worktree != "" || failed.Branch != "" {
		t.Fatalf("recovered = %+v, want no checkout", failed)
	}
}

func TestNoWorktreeRejectsABadDirectoryOrRef(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	file := filepath.Join(resolvedTemp(t), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(resolvedTemp(t), "missing")
	_, statErr := os.Stat(missing)
	for _, tc := range []struct {
		name, repo, ref, msg string
	}{
		{name: "ref", repo: h.repo, ref: "HEAD", msg: "ref cannot be used with --no-worktree"},
		{name: "file", repo: file, msg: fmt.Sprintf("repo %q is not a directory", file)},
		{name: "missing", repo: missing, msg: statErr.Error()},
		{name: "relative", repo: "repo", msg: `repo "repo" is not an absolute path`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.call(ipc.Request{
				Method: ipc.MethodLaunch, Repo: tc.repo, Prompt: "hi", Ref: tc.ref, NoWorktree: true,
			})
			assertInvalidMessage(t, err, tc.msg)
		})
	}
	if runs := h.list(); len(runs) != 0 {
		t.Fatalf("launch created runs: %+v", runs)
	}
	if branches := clbBranches(t, h.repo); branches != "" {
		t.Fatalf("launch created branches:\n%s", branches)
	}
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func gitHEAD(t *testing.T, repo string) string {
	t.Helper()
	return gitOutput(t, repo, "rev-parse", "HEAD")
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
