// Package worktree creates and removes the git worktree each run works in.
package worktree

import "context"

// Root returns the top-level directory of the git repository containing dir.
func Root(ctx context.Context, dir string) (string, error) {
	panic("not implemented")
}

// Add creates a worktree of repo at path on a new branch that starts at
// baseRef. If path is already a worktree of repo, Add does nothing. If the
// branch exists from an earlier attempt, Add checks it out instead.
func Add(ctx context.Context, repo, path, branch, baseRef string) error {
	panic("not implemented")
}

// Remove deletes the worktree at path, discarding uncommitted changes, and
// prunes git's record of it. The branch and its commits stay. Removing a
// worktree that no longer exists succeeds.
func Remove(ctx context.Context, repo, path string) error {
	panic("not implemented")
}
