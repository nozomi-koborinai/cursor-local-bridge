package daemon_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
