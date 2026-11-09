package worktree

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAddCreatesBranchAtBase(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()

	root, err := Root(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(t, root, repo) {
		t.Fatalf("root = %s, want %s", root, repo)
	}
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	subRoot, err := Root(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(t, subRoot, repo) {
		t.Fatalf("subdir root = %s, want %s", subRoot, repo)
	}

	path := filepath.Join(t.TempDir(), "nested", "wt")
	const branch = "clb/run-1"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s", got, head)
	}
	if got := gitOut(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("branch = %s, want %s", got, branch)
	}
	if got := gitOut(t, repo, "rev-parse", branch); got != head {
		t.Fatalf("branch tip = %s, want %s", got, head)
	}
	if got := gitOut(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("main checkout is on %s", got)
	}
}

func TestAddTwiceChangesNothing(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-2"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(path, "note.txt")
	if err := os.WriteFile(note, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, repo, path)
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, repo, path); after != before {
		t.Fatalf("before:\n%s\nafter:\n%s", before, after)
	}
	body, err := os.ReadFile(note)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep\n" {
		t.Fatalf("note = %q", body)
	}
}

func TestRemoveDeletesWorktree(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-3"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
	list := gitOut(t, repo, "worktree", "list", "--porcelain")
	if strings.Contains(list, path) || strings.Contains(list, resolved) {
		t.Fatalf("still listed:\n%s", list)
	}
	if got := gitOut(t, repo, "rev-parse", branch); got != head {
		t.Fatalf("branch tip = %s, want %s", got, head)
	}
}

func TestRemoveTwice(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	if err := Add(ctx, repo, path, "clb/run-4", head); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveSavesUncommittedWork(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-save"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("secret.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "add", ".gitignore")
	gitOut(t, path, "commit", "-m", "ignore rules")
	writeHook(t, path, "#!/bin/sh\necho hook-refused >&2\nexit 1\n")
	gitOut(t, path, "config", "commit.gpgsign", "true")
	gitOut(t, path, "config", "gpg.program", "false")

	if err := os.WriteFile(filepath.Join(path, "README"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "note.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "secret.txt"), []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "add", "-A")
	if out, err := exec.Command("git", "-C", path, "commit", "-m", "blocked").CombinedOutput(); err == nil || !bytes.Contains(out, []byte("hook-refused")) {
		t.Fatalf("pre-commit hook did not refuse the commit: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", path, "commit", "--no-verify", "-m", "blocked").CombinedOutput(); err == nil || !bytes.Contains(out, []byte("gpg failed to sign")) {
		t.Fatalf("signing did not refuse the commit: %v\n%s", err, out)
	}

	if err := Remove(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree stat = %v, want it gone", err)
	}
	if msg := gitOut(t, repo, "log", "-1", "--format=%s", branch); msg != "clb: save uncommitted changes before removing the worktree" {
		t.Fatalf("commit message = %q", msg)
	}
	names := strings.Split(gitOut(t, repo, "ls-tree", "-r", "--name-only", branch), "\n")
	for _, want := range []string{".gitignore", "README", "note.txt"} {
		if !slices.Contains(names, want) {
			t.Fatalf("tree = %q, want %s", names, want)
		}
	}
	if slices.Contains(names, "secret.txt") {
		t.Fatalf("tree = %q, want secret.txt left out", names)
	}
	if body := gitOut(t, repo, "show", branch+":README"); body != "changed" {
		t.Fatalf("README = %q", body)
	}
	if body := gitOut(t, repo, "show", branch+":note.txt"); body != "new" {
		t.Fatalf("note.txt = %q", body)
	}
	changed := strings.Split(gitOut(t, repo, "diff-tree", "--no-commit-id", "--name-only", "-r", branch), "\n")
	if slices.Contains(changed, "secret.txt") || !slices.Contains(changed, "README") || !slices.Contains(changed, "note.txt") {
		t.Fatalf("saved commit changed %q", changed)
	}
}

func TestRemoveSavesWhenGitIdentityIsUnset(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		configure  bool
	}{
		{name: "fallback", want: "clb <clb@localhost>"},
		{name: "configured", want: "Ada Lovelace <ada@example.com>", configure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGitConfig(t)
			repo := initRepo(t)
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "wt")
			const branch = "clb/run-ident"
			head := gitOut(t, repo, "rev-parse", "HEAD")
			if err := Add(ctx, repo, path, branch, head); err != nil {
				t.Fatal(err)
			}
			if tc.configure {
				gitOut(t, path, "config", "user.name", "Ada Lovelace")
				gitOut(t, path, "config", "user.email", "ada@example.com")
			}
			if err := os.WriteFile(filepath.Join(path, "note.txt"), []byte("saved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Remove(ctx, repo, path); err != nil {
				t.Fatal(err)
			}
			if got := gitOut(t, repo, "log", "-1", "--format=%an <%ae>", branch); got != tc.want {
				t.Fatalf("author = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRemoveLeavesTheWorktreeWhenTheSaveFails(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-lock"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "README"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDir := gitOut(t, path, "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gitDir, "index.lock"), []byte("locked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Remove(ctx, repo, path)
	if err == nil {
		t.Fatal("Remove succeeded while index.lock was held")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("worktree stat = %v, want it kept after %v", statErr, err)
	}
	if got := gitOut(t, repo, "rev-parse", branch); got != head {
		t.Fatalf("branch tip = %s, want %s", got, head)
	}
	body, readErr := os.ReadFile(filepath.Join(path, "README"))
	if readErr != nil || string(body) != "changed\n" {
		t.Fatalf("README = %q, %v", body, readErr)
	}
}

func TestCommitSurvivesRemove(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-6"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(keep, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "add", "keep.txt")
	gitOut(t, path, "commit", "-m", "keep")
	sha := gitOut(t, path, "rev-parse", "HEAD")
	if sha == head {
		t.Fatal("commit did not move HEAD")
	}
	if err := Remove(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
	if got := gitOut(t, repo, "rev-parse", branch); got != sha {
		t.Fatalf("branch tip = %s, want %s", got, sha)
	}
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, path, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("HEAD = %s, want %s", got, sha)
	}
	body, err := os.ReadFile(keep)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "kept\n" {
		t.Fatalf("keep.txt = %q", body)
	}
}

func TestAddTwiceThroughSymlink(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	real := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/link"
	if err := Add(ctx, repo, real, branch, head); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, repo, link, branch, head); err != nil {
		t.Fatalf("add via symlink: %v", err)
	}
	if got := gitOut(t, link, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s", got, head)
	}
	if gitOut(t, repo, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatal("main checkout moved")
	}
}

func TestAddErrorIncludesGitOutput(t *testing.T) {
	repo, _ := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	err := Add(t.Context(), repo, path, "clb/missing-base", "not-a-ref")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does not name a commit") || !strings.Contains(msg, "not-a-ref") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree path stat = %v, want it absent", statErr)
	}
}

func TestResolveCommit(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	for _, ref := range []string{"HEAD", "main"} {
		got, err := ResolveCommit(ctx, repo, ref)
		if err != nil {
			t.Fatalf("ResolveCommit(%q): %v", ref, err)
		}
		if got != head {
			t.Fatalf("ResolveCommit(%q) = %s, want %s", ref, got, head)
		}
	}
}

func TestAddRecreatesADeletedWorktree(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-again"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "add", "keep.txt")
	gitOut(t, path, "commit", "-m", "keep")
	sha := gitOut(t, path, "rev-parse", "HEAD")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := Add(ctx, repo, path, branch, "HEAD"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, path, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("HEAD = %s, want %s", got, sha)
	}
	body, err := os.ReadFile(filepath.Join(path, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "kept\n" {
		t.Fatalf("keep.txt = %q", body)
	}
}

func TestAddRejectsABaseRefThatIsNotACommit(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := t.Context()
	for _, tc := range []struct{ ref, branch string }{
		{"--detach", "clb/detach"},
		{"-b", "clb/dash-b"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wt")
			err := Add(ctx, repo, path, tc.branch, tc.ref)
			if err == nil || !strings.Contains(err.Error(), "does not name a commit") {
				t.Fatalf("Add(%q) = %v, want an unknown commit", tc.ref, err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("worktree path stat = %v, want it absent", statErr)
			}
			cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+tc.branch)
			if cmd.Run() == nil {
				t.Fatalf("branch %s was created", tc.branch)
			}
		})
	}
}

// git update-ref accepts a branch named "-q", which worktree add would read
// as its quiet flag and then start the branch at HEAD.
func TestAddStartsAtABaseRefThatLooksLikeAFlag(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	gitOut(t, repo, "commit", "--allow-empty", "-m", "second")
	gitOut(t, repo, "update-ref", "refs/heads/-q", head)
	path := filepath.Join(t.TempDir(), "wt")
	if err := Add(ctx, repo, path, "clb/flag", "-q"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s, the commit -q names", got, head)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	ok, err := Exists(dir)
	if err != nil || !ok {
		t.Fatalf("dir = %v, %v; want present", ok, err)
	}
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = Exists(file)
	if err != nil || !ok {
		t.Fatalf("file = %v, %v; want present", ok, err)
	}
	ok, err = Exists(filepath.Join(dir, "missing"))
	if err != nil || ok {
		t.Fatalf("missing = %v, %v; want absent", ok, err)
	}
}

func TestBranchExists(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := t.Context()
	ok, err := BranchExists(ctx, repo, "")
	if err != nil || ok {
		t.Fatalf("empty name = %v, %v; want absent", ok, err)
	}
	ok, err = BranchExists(ctx, repo, "main")
	if err != nil || !ok {
		t.Fatalf("main = %v, %v; want present", ok, err)
	}
	ok, err = BranchExists(ctx, repo, "no-such")
	if err != nil || ok {
		t.Fatalf("missing = %v, %v; want absent", ok, err)
	}
}

func TestRemoveEmptyPath(t *testing.T) {
	if err := Remove(t.Context(), t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardDropsUncommittedWork(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-discard"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "note.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Discard(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v, want the worktree gone", err)
	}
	if got := gitOut(t, repo, "rev-parse", branch); got != head {
		t.Fatalf("branch tip = %s, want %s with the dirty file dropped", got, head)
	}
	if names := gitOut(t, repo, "ls-tree", "-r", "--name-only", branch); strings.Contains(names, "note.txt") {
		t.Fatalf("tree = %q, want the dirty file left out", names)
	}
	if err := Discard(ctx, repo, path); err != nil {
		t.Fatalf("second discard: %v", err)
	}
}

func TestDiscardRemovesAnUnregisteredDirectory(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	if err := Add(ctx, repo, path, "clb/run-forgot", head); err != nil {
		t.Fatal(err)
	}
	aside := path + "-aside"
	if err := os.Rename(path, aside); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "worktree", "prune")
	if err := os.Rename(aside, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "left.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Discard(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v, want the unregistered directory gone", err)
	}
}

func TestDiscardEmptyPath(t *testing.T) {
	if err := Discard(t.Context(), t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteBranchRemovesCommitsAndIsIdempotent(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-delete"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "note.txt"), []byte("saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "add", "note.txt")
	gitOut(t, path, "commit", "-m", "note")
	if err := Discard(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	ok, err := BranchExists(ctx, repo, branch)
	if err != nil || !ok {
		t.Fatalf("branch before delete = %v, %v; want it present", ok, err)
	}
	if err := DeleteBranch(ctx, repo, branch); err != nil {
		t.Fatal(err)
	}
	ok, err = BranchExists(ctx, repo, branch)
	if err != nil || ok {
		t.Fatalf("branch after delete = %v, %v; want it gone", ok, err)
	}
	if err := DeleteBranch(ctx, repo, branch); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := DeleteBranch(ctx, repo, ""); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s", got, head)
	}
}

func writeHook(t *testing.T, worktree, body string) {
	t.Helper()
	hook := gitOut(t, worktree, "rev-parse", "--git-path", "hooks/pre-commit")
	if !filepath.IsAbs(hook) {
		hook = filepath.Join(worktree, hook)
	}
	if err := os.MkdirAll(filepath.Dir(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "no-gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, key := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
		"EMAIL",
	} {
		unsetEnv(t, key)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	val, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, val)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitOut(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "README")
	gitOut(t, repo, "-c", "user.name=setup", "-c", "user.email=setup@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "init")
	return repo
}

func newRepo(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "clb")
	t.Setenv("GIT_AUTHOR_EMAIL", "clb@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "clb")
	t.Setenv("GIT_COMMITTER_EMAIL", "clb@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	repo := t.TempDir()
	gitOut(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "README")
	gitOut(t, repo, "commit", "-m", "init")
	return repo, gitOut(t, repo, "rev-parse", "HEAD")
}

func snapshot(t *testing.T, repo, path string) string {
	t.Helper()
	return strings.Join([]string{
		gitOut(t, path, "rev-parse", "HEAD"),
		gitOut(t, path, "status", "--porcelain"),
		gitOut(t, repo, "worktree", "list", "--porcelain"),
		gitOut(t, repo, "rev-parse", "--abbrev-ref", "HEAD"),
	}, "\n")
}

func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	aa, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	return aa == bb
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
