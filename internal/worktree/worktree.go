// Package worktree creates and removes the git worktree each run works in.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Root returns the top-level directory of the git repository containing dir.
func Root(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Add creates a worktree of repo at path on a new branch that starts at
// baseRef. baseRef must name a commit. Add passes that commit to git, not
// the caller's string, so a string that starts with '-' cannot be parsed as
// a flag. If path is already a worktree of repo, Add does nothing. If the
// branch exists from an earlier attempt, Add checks it out instead.
func Add(ctx context.Context, repo, path, branch, baseRef string) error {
	registered, err := isWorktree(ctx, repo, path)
	if err != nil {
		return err
	}
	if registered {
		return nil
	}
	exists, err := branchExists(ctx, repo, branch)
	if err != nil {
		return err
	}
	var commit string
	if !exists {
		commit, err = ResolveCommit(ctx, repo, baseRef)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if exists {
		_, err = git(ctx, repo, "worktree", "add", path, branch)
		return err
	}
	_, err = git(ctx, repo, "worktree", "add", "-b", branch, path, commit)
	return err
}

// ResolveCommit returns the commit hash ref names. ref is one argument after
// --end-of-options, so a value that starts with '-' is a revision, not a flag.
func ResolveCommit(ctx context.Context, repo, ref string) (string, error) {
	spec := ref + "^{commit}"
	out, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "--end-of-options", spec)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", fmt.Errorf("%s does not name a commit: %w", ref, err)
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Remove deletes the worktree at path, discarding uncommitted changes, and
// prunes git's record of it. The branch and its commits stay. Removing a
// worktree that no longer exists succeeds.
func Remove(ctx context.Context, repo, path string) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err := git(ctx, repo, "worktree", "remove", "--force", path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := git(ctx, repo, "worktree", "prune")
	return err
}

// git stores the worktree path after resolving symlinks. macOS temp dirs live
// under /var, which points at /private/var, so the two spellings name one path.
func isWorktree(ctx context.Context, repo, path string) (bool, error) {
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	out, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	const prefix = "worktree "
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		got, err := filepath.EvalSymlinks(strings.TrimSpace(line[len(prefix):]))
		if err != nil {
			continue
		}
		if got == want {
			return true, nil
		}
	}
	return false, nil
}

func branchExists(ctx context.Context, repo, branch string) (bool, error) {
	ref := "refs/heads/" + branch
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "show-ref", "--verify", "--quiet", ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, gitErr([]string{"show-ref", "--verify", "--quiet", ref}, err, stderr.String())
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", gitErr(args, err, stderr.String())
	}
	return stdout.String(), nil
}

func gitErr(args []string, err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	sub := strings.Join(args, " ")
	if msg == "" {
		return fmt.Errorf("git %s: %w", sub, err)
	}
	return fmt.Errorf("git %s: %s: %w", sub, msg, err)
}
