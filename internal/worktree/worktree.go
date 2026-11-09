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
// directory is gone but git still lists it, Add prunes that record first.
// Git refuses to add a path it still has registered. If the branch exists
// from an earlier attempt, Add checks it out instead.
func Add(ctx context.Context, repo, path, branch, baseRef string) error {
	registered, err := isWorktree(ctx, repo, path)
	if err != nil {
		return err
	}
	if registered {
		return nil
	}
	exists, err := BranchExists(ctx, repo, branch)
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
	if _, err := git(ctx, repo, "worktree", "prune"); err != nil {
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

// Remove deletes the worktree at path and prunes git's record of it. The
// branch and its commits stay. Changes that are not committed yet are
// committed onto the worktree's branch first. Ignored files are left out of
// that commit. If the commit fails, the worktree stays. Removing a worktree
// that no longer exists succeeds. An empty path does nothing, so a run that
// has no worktree cannot prune or delete the directory it was launched in.
func Remove(ctx context.Context, repo, path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		if err := saveUncommitted(ctx, path); err != nil {
			return err
		}
		if _, err := git(ctx, repo, "worktree", "remove", "--force", path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := git(ctx, repo, "worktree", "prune")
	return err
}

// Discard removes the worktree at path without committing. Clean deletes the
// branch as well, so a save commit would be thrown away, and a hook or a
// missing identity would refuse the commit and leave the worktree in place.
// An empty path does nothing. A path git no longer lists is deleted from
// disk. A missing path succeeds. Discard then prunes git's worktree records,
// including a record left behind when an earlier discard crashed.
func Discard(ctx context.Context, repo, path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		if _, rmErr := git(ctx, repo, "worktree", "remove", "--force", path); rmErr != nil {
			registered, err := isWorktree(ctx, repo, path)
			if err != nil {
				return err
			}
			if registered {
				return rmErr
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := git(ctx, repo, "worktree", "prune")
	return err
}

// DeleteBranch deletes branch. An empty name, or a branch that is already
// gone, succeeds. Git refuses to delete a branch that is still checked out,
// so the caller removes the worktree first.
func DeleteBranch(ctx context.Context, repo, branch string) error {
	if branch == "" {
		return nil
	}
	exists, err := BranchExists(ctx, repo, branch)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = git(ctx, repo, "branch", "-D", "--end-of-options", branch)
	return err
}

// Exists reports whether path is present. A missing path is false, not an error.
func Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

const saveMessage = "clb: save uncommitted changes before removing the worktree"

// saveUncommitted commits a dirty worktree onto its HEAD. The daemon has no
// terminal, so the commit skips hooks and signing. Those would fail the save
// and leave the worktree in place.
func saveUncommitted(ctx context.Context, path string) error {
	out, err := git(ctx, path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	if _, err := git(ctx, path, "add", "-A"); err != nil {
		return err
	}
	args := []string{"-c", "commit.gpgsign=false"}
	missing, err := identityMissing(ctx, path)
	if err != nil {
		return err
	}
	if missing {
		args = append(args, "-c", "user.name=clb", "-c", "user.email=clb@localhost")
	}
	args = append(args, "commit", "--no-verify", "-m", saveMessage)
	_, err = git(ctx, path, args...)
	return err
}

func identityMissing(ctx context.Context, path string) (bool, error) {
	for _, key := range []string{"user.name", "user.email"} {
		value, ok, err := gitConfig(ctx, path, key)
		if err != nil {
			return false, err
		}
		if !ok || value == "" {
			return true, nil
		}
	}
	return false, nil
}

func gitConfig(ctx context.Context, dir, key string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", key)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", false, nil
	}
	return "", false, gitErr([]string{"config", "--get", key}, err, stderr.String())
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

// BranchExists reports whether repo has a local branch of that name.
// An empty name does not.
func BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	if branch == "" {
		return false, nil
	}
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
