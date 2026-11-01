//go:build linux

package instance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/microvm"
)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// prepareWorkspace decides what /workspace starts as and returns a host
// directory to copy in ("" for none).
//
// Inside a git repository it creates branch persona/<instance> at HEAD with
// its own worktree under $PERSONA_HOME/worktrees, and returns a standalone
// single-branch clone of it, whose origin points at the repo's real remote.
// The VM never sees the host repo's other branches or its .git directory.
func prepareWorkspace(ctx context.Context, rec *record, workdir string, log io.Writer) (string, func(), error) {
	noop := func() {}
	if workdir == "" {
		return "", noop, nil
	}
	root, err := git(ctx, workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		rec.HostDir = workdir
		return workdir, noop, nil
	}
	if rel, err := filepath.Rel(root, workdir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		rec.Cwd = guestWorkspace + "/" + filepath.ToSlash(rel)
	}
	if out, _ := git(ctx, root, "status", "--porcelain"); out != "" {
		fmt.Fprintf(log, "warning: %s has uncommitted changes; the instance starts from the last commit\n", root)
	}
	rec.Repo = root
	rec.Branch = "persona/" + rec.Instance
	rec.Base, _ = git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if rec.Base == "" {
		rec.Base = "main"
	}
	rec.Worktree = filepath.Join(config.WorktreesDir(), rec.VMID)
	if err := os.MkdirAll(config.WorktreesDir(), 0o755); err != nil {
		return "", noop, err
	}
	if _, err := git(ctx, root, "worktree", "add", "-q", "-b", rec.Branch, rec.Worktree, "HEAD"); err != nil {
		rec.Worktree = "" // nothing to clean up
		return "", noop, fmt.Errorf("create branch %s: %w", rec.Branch, err)
	}
	tmp, err := os.MkdirTemp("", "persona-clone-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	clone := filepath.Join(tmp, "ws")
	if _, err := git(ctx, root, "clone", "-q", "--single-branch", "--branch", rec.Branch, "file://"+root, clone); err != nil {
		cleanup()
		return "", noop, err
	}
	if url, err := git(ctx, root, "remote", "get-url", "origin"); err == nil && url != "" {
		_, err = git(ctx, clone, "remote", "set-url", "origin", url)
	} else {
		_, err = git(ctx, clone, "remote", "remove", "origin")
	}
	if err != nil {
		cleanup()
		return "", noop, err
	}
	return clone, cleanup, nil
}

// fetchBranch brings the instance branch's commits out of the VM as a git
// bundle, verifies them, and fast-forwards the worktree. Only the instance's
// own branch is ever read, whatever the guest has done to its refs.
func fetchBranch(ctx context.Context, rec *record, vm *microvm.VM) (string, error) {
	ref := "refs/heads/" + rec.Branch
	conn, err := vm.Agent.Stream(ctx, agent.Exec{Argv: []string{"git", "bundle", "create", "-", ref},
		User: rec.User, Env: rec.Env, Dir: guestWorkspace})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "persona-*.bundle")
	if err != nil {
		conn.Close()
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, io.LimitReader(conn, 4<<30))
	conn.Close()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("read bundle: %w", err)
	}
	if fi, _ := os.Stat(f.Name()); fi == nil || fi.Size() == 0 {
		return "", fmt.Errorf("the VM has no branch %s to push (was it renamed or deleted?)", rec.Branch)
	}
	if _, err := git(ctx, rec.Worktree, "-c", "fetch.fsckObjects=true", "fetch", "-q", f.Name(), ref); err != nil {
		return "", err
	}
	if _, err := git(ctx, rec.Worktree, "merge", "-q", "--ff-only", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("%s has diverged from the VM's commits (did someone commit on the host?): %w", rec.Branch, err)
	}
	return git(ctx, rec.Worktree, "rev-parse", "HEAD")
}
