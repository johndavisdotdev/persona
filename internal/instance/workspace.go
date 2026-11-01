//go:build linux

package instance

import (
	"context"
	"errors"
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
		// Outside a repository nothing is copied: it may be a home directory
		// of many gigabytes, and there is no branch to bring the work back on.
		fmt.Fprintf(log, "%s is not in a git repository: /workspace starts empty\n", workdir)
		return "", noop, nil
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
	if err := os.MkdirAll(config.WorktreesDir(), 0o755); err != nil {
		return "", noop, err
	}
	if err := attachWorktree(ctx, rec, root, log); err != nil {
		rec.Worktree = "" // nothing to clean up
		return "", noop, err
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
	// The pull request base comes along as host/<base>, so the spawn can
	// rebase on it from the start (persona-agent fetch refreshes it).
	if rec.Base != rec.Branch {
		if _, err := git(ctx, root, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Base); err == nil {
			if _, err := git(ctx, clone, "fetch", "-q", "file://"+root, "refs/heads/"+rec.Base+":"+hostRef(rec.Base)); err != nil {
				cleanup()
				return "", noop, err
			}
		}
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

// hostRef is where the host's branch lands in a spawn's repository.
func hostRef(branch string) string { return "refs/remotes/host/" + branch }

// fetchIntoVM pushes branches of the host repository into the spawn's
// repository as host/<branch>, for persona-agent fetch: over git's own
// protocol (only missing objects travel), through persona __vm-rsh, as the
// session user. A branch is a host branch, or one of the host's
// remote-tracking branches ("origin/main"); none means the PR base. The
// spawn gets copies to read; nothing it does changes the host's refs.
func fetchIntoVM(ctx context.Context, rec *record, vm *microvm.VM, branches []string) (agent.FetchResult, error) {
	if rec.Repo == "" {
		return agent.FetchResult{}, errors.New("this spawn has no git workspace (it wasn't spawned in a repository)")
	}
	if len(branches) == 0 {
		branches = []string{rec.Base}
	}
	if len(branches) > 32 {
		return agent.FetchResult{}, errors.New("too many branches (at most 32 at a time)")
	}
	var res agent.FetchResult
	var specs []string
	for _, b := range branches {
		if b == "" || strings.HasPrefix(b, "-") {
			return agent.FetchResult{}, fmt.Errorf("invalid branch %q", b)
		}
		if _, err := git(ctx, rec.Repo, "check-ref-format", "--branch", b); err != nil {
			return agent.FetchResult{}, fmt.Errorf("invalid branch %q", b)
		}
		src := ""
		for _, ref := range []string{"refs/heads/" + b, "refs/remotes/" + b} {
			if _, err := git(ctx, rec.Repo, "rev-parse", "--verify", "-q", ref); err == nil {
				src = ref
				break
			}
		}
		if src == "" {
			return agent.FetchResult{}, fmt.Errorf("the host repository has no branch %q", b)
		}
		commit, err := git(ctx, rec.Repo, "rev-parse", src)
		if err != nil {
			return agent.FetchResult{}, err
		}
		specs = append(specs, "+"+src+":"+hostRef(b))
		res.Refs = append(res.Refs, agent.FetchedRef{Branch: b, Ref: "host/" + b, Commit: commit})
	}
	self, err := os.Executable()
	if err != nil {
		return agent.FetchResult{}, err
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", rec.Repo, "push", "--quiet", "--no-verify",
		"vm:" + guestWorkspace}, specs...)...)
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND="+shellQuote(self)+" __vm-rsh "+shellQuote(vm.Vsock)+" "+shellQuote(rec.User),
		"GIT_SSH_VARIANT=simple", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return agent.FetchResult{}, fmt.Errorf("git push into the spawn: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return res, nil
}

// attachWorktree gives rec its branch and worktree. A spawn killed earlier
// under the same name leaves its branch (always) and its worktree (when it
// had uncommitted changes); a new spawn of that name picks up from them
// instead of failing:
//
//   - the branch has a worktree: that worktree is used as is;
//   - the branch exists without one: it is checked out into a new worktree;
//   - neither exists: the branch is created at HEAD, with a new worktree.
func attachWorktree(ctx context.Context, rec *record, root string, log io.Writer) error {
	_, _ = git(ctx, root, "worktree", "prune") // forget worktrees whose directory is gone
	path, err := worktreeOf(ctx, root, rec.Branch)
	if err != nil {
		return err
	}
	switch {
	case path == root:
		return fmt.Errorf("branch %s is checked out in %s; switch that checkout to another branch first", rec.Branch, root)
	case path != "":
		rec.Worktree = path
		fmt.Fprintf(log, "Reusing the worktree of branch %s at %s\n", rec.Branch, path)
		if out, _ := git(ctx, path, "status", "--porcelain"); out != "" {
			fmt.Fprintf(log, "warning: %s has uncommitted changes; they stay there, and the spawn starts from the branch's last commit\n", path)
		}
		return nil
	}
	rec.Worktree = freeWorktreePath(rec.VMID)
	if _, err := git(ctx, root, "rev-parse", "--verify", "-q", "refs/heads/"+rec.Branch); err == nil {
		if _, err := git(ctx, root, "worktree", "add", "-q", rec.Worktree, rec.Branch); err != nil {
			return fmt.Errorf("check out branch %s: %w", rec.Branch, err)
		}
		head, _ := git(ctx, root, "rev-parse", "--short", "refs/heads/"+rec.Branch)
		fmt.Fprintf(log, "Reusing branch %s (at %s, not your current HEAD)\n", rec.Branch, head)
		return nil
	}
	if _, err := git(ctx, root, "worktree", "add", "-q", "-b", rec.Branch, rec.Worktree, "HEAD"); err != nil {
		return fmt.Errorf("create branch %s: %w", rec.Branch, err)
	}
	return nil
}

// worktreeOf returns the path of the worktree that has branch checked out
// (the main checkout's path if it is there), or "".
func worktreeOf(ctx context.Context, root, branch string) (string, error) {
	out, err := git(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			path = p
		} else if line == "branch refs/heads/"+branch {
			return path, nil
		}
	}
	return "", nil
}

// freeWorktreePath is where a new worktree goes: <worktrees>/<vm id>, or
// with a suffix if a directory (not a registered worktree) is in the way.
func freeWorktreePath(vmID string) string {
	base := filepath.Join(config.WorktreesDir(), vmID)
	p := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
}

// fetchBranch brings the instance branch's commits out of the VM as a git
// bundle, verifies them, and fast-forwards the worktree. Only the instance's
// own branch is ever read, whatever the guest has done to its refs.
// How fetchBranch updates the host worktree with the VM's branch.
const (
	fastForward = iota // only new commits on top
	keepChanges        // take the VM's history; refuse to clobber uncommitted changes
	replaceAll         // take the VM's history and files (saveWork rsyncs them next)
)

func fetchBranch(ctx context.Context, rec *record, vm *microvm.VM, mode int) (string, error) {
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
	switch mode {
	case keepChanges:
		// The spawn rewrote its branch (a rebase): take its history. --keep
		// refuses, rather than lose them, if uncommitted changes in the
		// worktree would be overwritten.
		if _, err := git(ctx, rec.Worktree, "reset", "-q", "--keep", "FETCH_HEAD"); err != nil {
			return "", fmt.Errorf("replacing %s with the VM's history: %w", rec.Branch, err)
		}
	case replaceAll:
		if _, err := git(ctx, rec.Worktree, "reset", "-q", "--hard", "FETCH_HEAD"); err != nil {
			return "", fmt.Errorf("replacing %s with the VM's history: %w", rec.Branch, err)
		}
	default:
		if _, err := git(ctx, rec.Worktree, "merge", "-q", "--ff-only", "FETCH_HEAD"); err != nil {
			return "", fmt.Errorf("%s has diverged from the VM's commits (after a rebase, push with --force): %w", rec.Branch, err)
		}
	}
	return git(ctx, rec.Worktree, "rev-parse", "HEAD")
}
