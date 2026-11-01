//go:build linux

package instance

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johndavisdev/persona/internal/config"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAttachWorktreeReuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if err := config.Init("", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	gitT(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0o644)
	gitT(t, root, "add", "a")
	gitT(t, root, "commit", "-q", "-m", "first")
	newRec := func() *record {
		return &record{Instance: "jill", VMID: "persona-p-jill", Branch: "persona/jill", Repo: root}
	}

	// Fresh: branch and worktree are created.
	r1 := newRec()
	if err := attachWorktree(ctx, r1, root, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The spawn was killed with uncommitted changes: worktree and branch kept.
	os.WriteFile(filepath.Join(r1.Worktree, "dirty"), []byte("x"), 0o644)
	r2 := newRec()
	if err := attachWorktree(ctx, r2, root, io.Discard); err != nil {
		t.Fatalf("second spawn with the worktree still there: %v", err)
	}
	if r2.Worktree != r1.Worktree {
		t.Errorf("reused worktree %s, want %s", r2.Worktree, r1.Worktree)
	}

	// The worktree was removed but the branch kept, with a commit of its own.
	gitT(t, r1.Worktree, "add", "dirty")
	gitT(t, r1.Worktree, "commit", "-q", "-m", "work")
	work := gitT(t, root, "rev-parse", "refs/heads/persona/jill")
	gitT(t, root, "worktree", "remove", r1.Worktree)
	r3 := newRec()
	if err := attachWorktree(ctx, r3, root, io.Discard); err != nil {
		t.Fatalf("spawn with only the branch left: %v", err)
	}
	if got := gitT(t, r3.Worktree, "rev-parse", "HEAD"); got != work {
		t.Errorf("worktree at %s, want the branch's own commit %s", got, work)
	}

	// A plain directory in the way gets a suffixed path instead.
	gitT(t, root, "worktree", "remove", r3.Worktree)
	os.MkdirAll(filepath.Join(config.WorktreesDir(), "persona-p-jill"), 0o755)
	r4 := newRec()
	if err := attachWorktree(ctx, r4, root, io.Discard); err != nil {
		t.Fatalf("with a stray directory: %v", err)
	}
	if !strings.HasSuffix(r4.Worktree, "persona-p-jill-2") {
		t.Errorf("worktree %s", r4.Worktree)
	}

	// The branch checked out in the main checkout can't be reused.
	gitT(t, root, "worktree", "remove", r4.Worktree)
	gitT(t, root, "checkout", "-q", "persona/jill")
	if err := attachWorktree(ctx, newRec(), root, io.Discard); err == nil || !strings.Contains(err.Error(), "checked out in") {
		t.Errorf("branch checked out in the repo: %v", err)
	}
}
