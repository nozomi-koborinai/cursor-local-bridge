package worktree

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestRemoveDiscardsUncommittedChanges(t *testing.T) {
	repo, head := newRepo(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "clb/run-5"
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "dirty.txt"), []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "README"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, repo, path); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, repo, "rev-parse", branch); got != head {
		t.Fatalf("branch tip = %s, want %s", got, head)
	}
	if err := Add(ctx, repo, path, branch, head); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "dirty.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dirty file stat = %v", err)
	}
	body, err := os.ReadFile(filepath.Join(path, "README"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hi\n" {
		t.Fatalf("README = %q", body)
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

func TestAddErrorIncludesGitOutput(t *testing.T) {
	repo, _ := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	err := Add(t.Context(), repo, path, "clb/missing-base", "not-a-ref")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "worktree add") || !strings.Contains(msg, "not-a-ref") {
		t.Fatalf("err = %v", err)
	}
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
