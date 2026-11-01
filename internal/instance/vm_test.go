//go:build linux

package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/microvm"
	"github.com/johndavisdev/persona/internal/persona"
)

func TestVMID(t *testing.T) {
	if got := vmID("persona-reviewer-swift-otter"); got != "persona-reviewer-swift-otter" {
		t.Errorf("short name changed: %s", got)
	}
	long := "persona-" + strings.Repeat("a", 48) + "-" + strings.Repeat("b", 40)
	id := vmID(long)
	if !vmIDRe.MatchString(id) || id == vmID(long+"c") {
		t.Errorf("vmID(long) = %q", id)
	}
}

func TestAllocIP(t *testing.T) {
	ip, err := allocIP([]*record{{IP: "10.200.1.1"}, {IP: "10.200.1.2"}})
	if err != nil || ip.String() != "10.200.1.3" {
		t.Fatalf("allocIP = %v %v", ip, err)
	}
}

// sessionDockerfile stands in for a persona image: same user, workspace and
// entrypoint contract, but a shell instead of Claude Code.
const sessionDockerfile = `FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git rsync tmux ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash --uid 1000 claude && mkdir -p /workspace && chown claude:claude /workspace
USER claude
ENV PATH=/home/claude/.local/bin:$PATH
COPY --chown=claude:claude --chmod=0755 <<'EOF' /home/claude/.local/bin/persona-entrypoint
#!/bin/sh
tmux -u new-session -d -s ` + persona.SessionName + ` 'printf %s "$PERSONA_TEST_SECRET" > /tmp/secret-seen; pwd > /tmp/cwd-seen; exec sh'
while tmux has-session -t ` + persona.SessionName + ` 2>/dev/null; do sleep 1; done
EOF
WORKDIR /workspace
ENTRYPOINT ["/home/claude/.local/bin/persona-entrypoint"]
`

// TestVMRuntime spawns an instance from a git repository and exercises the
// branch, push broker, pull requests, and stop/remove.
//
//	PERSONA_FC_TEST=1 PERSONA_FC_ASSETS=$HOME/.config/persona/firecracker go test ./internal/instance -v
func TestVMRuntime(t *testing.T) {
	if os.Getenv("PERSONA_FC_TEST") == "" {
		t.Skip("set PERSONA_FC_TEST=1 (requires the persona package installed and Docker)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Persona's home must share a filesystem with /srv/jailer (hard links).
	home, err := os.MkdirTemp(os.Getenv("PERSONA_FC_ASSETS"), "test-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := config.Init("", home); err != nil {
		t.Fatal(err)
	}
	viper.Set(config.KeyRuntime, "firecracker")

	tmp := t.TempDir()
	agentBin := filepath.Join(tmp, "persona-agent")
	build := exec.CommandContext(ctx, "go", "build", "-o", agentBin, "github.com/johndavisdev/persona/cmd/persona-agent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(tmp, "Dockerfile"), []byte(sessionDockerfile), 0o644)
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", "persona-test-session", tmp).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}

	// A repo whose origin is a local bare repository, and a fake gh.
	origin := filepath.Join(tmp, "origin.git")
	repo := filepath.Join(tmp, "repo")
	sh := func(dir, script string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	sh(tmp, "git init -q --bare origin.git && git init -q -b main repo")
	sh(repo, "git config user.name 'Test User' && git config user.email test@example.com && "+
		"mkdir sub && echo hello > README && echo x > sub/file && git add -A && git commit -qm init && "+
		"git remote add origin "+origin+" && git push -q origin main")
	gh := filepath.Join(tmp, "gh")
	ghLog := filepath.Join(tmp, "gh.log")
	os.WriteFile(gh, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+ghLog+"\necho https://github.com/example/repo/pull/7\n"), 0o755)

	self, _ := os.Executable()
	os.Setenv("PERSONA_TEST_RSH", "1")
	os.Setenv("PERSONA_TEST_SECRET", "s3cret value's")
	v := NewVM()
	v.Agent, v.Self, v.Rsh = agentBin, "", shellQuote(self)+" -test.run=^$"
	v.Kernel = viper.GetString(config.KeyVMKernel)
	v.MemMiB, v.DiskGiB = 1024, 8
	if err := v.Available(ctx); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	name, err := v.Run(ctx, RunOptions{Persona: "tester", Instance: "t1", Image: "persona-test-session",
		Workdir: filepath.Join(repo, "sub"), Env: []string{"PERSONA_TEST_SECRET"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("spawned %s in %v", name, time.Since(t0).Round(time.Millisecond))
	rec, _ := loadRecord(name)
	t.Cleanup(func() {
		if t.Failed() {
			if vm, ok := microvm.Lookup(rec.VMID); ok {
				t.Logf("console:\n%s", vm.ConsoleLog())
			}
		}
		microvm.Stop(context.Background(), rec.VMID)
	})
	guest := func(cmd string) (string, error) {
		out, err := v.Exec(ctx, name, "sh", "-c", cmd)
		return strings.TrimSpace(string(out)), err
	}
	must := func(cmd string) string {
		t.Helper()
		out, err := guest(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return out
	}

	cs, err := v.List(ctx, "")
	if err != nil || len(cs) != 1 || !cs[0].Running() || !strings.Contains(cs[0].Status, "persona/t1") {
		t.Fatalf("List = %+v %v", cs, err)
	}
	if out := must("git -C /workspace branch --show-current; stat -c %U /workspace/README; git -C /workspace remote get-url origin; git -C /workspace for-each-ref --format='%(refname)'"); out != "persona/t1\nclaude\n"+origin+"\nrefs/heads/persona/t1\nrefs/remotes/origin/persona/t1" {
		t.Errorf("guest workspace: %q", out)
	}
	if out, _ := sh(repo, "git worktree list --porcelain"), ""; !strings.Contains(out, rec.Worktree) {
		t.Errorf("no host worktree %s:\n%s", rec.Worktree, out)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := v.Exec(ctx, name, "tmux", "has-session", "-t", persona.SessionName); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("no tmux session: %v", err)
		}
	}
	// The session's first command writes these; the session can exist first.
	must("for i in $(seq 100); do [ -s /tmp/cwd-seen ] && exit 0; sleep 0.05; done; exit 1")
	if out := must("cat /tmp/secret-seen; echo; cat /tmp/cwd-seen; stat -c '%a %U' /run/persona/env"); out != "s3cret value's\n/workspace/sub\n600 claude" {
		t.Errorf("session env/cwd: %q", out)
	}
	if out := must("git config --global user.email; grep -c 'persona-agent push' ~/.claude/CLAUDE.md"); out != "test@example.com\n1" {
		t.Errorf("guest config: %q", out)
	}

	// Push and PR through the broker.
	if _, err := guest("persona-agent push"); err == nil || !strings.Contains(err.Error(), "no host broker") {
		t.Errorf("push without broker: %v", err)
	}
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel()
	go (&Broker{GH: gh, Log: log.New(os.Stderr, "broker: ", 0)}).Run(bctx)
	must("cd /workspace && echo hi > new.txt && git add -A && git commit -qm 'vm commit'")
	var out string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if out, err = guest("persona-agent push"); err == nil {
			break
		}
	}
	var res agent.PushResult
	if err != nil || json.Unmarshal([]byte(out), &res) != nil || res.Branch != "persona/t1" {
		t.Fatalf("push: %q %v", out, err)
	}
	if msg := sh(origin, "git log -1 --format=%s persona/t1"); msg != "vm commit" {
		t.Errorf("origin has %q", msg)
	}
	if b, _ := os.ReadFile(filepath.Join(rec.Worktree, "new.txt")); string(b) != "hi\n" {
		t.Errorf("worktree not fast-forwarded: %q", b)
	}
	out = must(`persona-agent pr --title "-starts with a dash" --body "made in a VM"`)
	if json.Unmarshal([]byte(out), &res) != nil || res.PR != "https://github.com/example/repo/pull/7" {
		t.Errorf("pr: %q", out)
	}
	if args, _ := os.ReadFile(ghLog); !strings.Contains(string(args), "--title=-starts with a dash\n") ||
		!strings.Contains(string(args), "--head=persona/t1\n") || !strings.Contains(string(args), "--base=main\n") {
		t.Errorf("gh args:\n%s", args)
	}
	// Other branches never leave the VM.
	must("cd /workspace && git checkout -qb sneaky && echo x > s && git add -A && git commit -qm sneaky && persona-agent push")
	if refs := sh(origin, "git for-each-ref --format='%(refname:short)'"); strings.Contains(refs, "sneaky") {
		t.Errorf("origin got another branch: %s", refs)
	}
	must("cd /workspace && git checkout -q persona/t1")
	if _, err := guest("persona-agent request delete-repo"); err == nil {
		t.Error("unknown request accepted")
	}

	// Stop saves uncommitted work; remove keeps the dirty worktree.
	must("echo draft > /workspace/draft.txt && mkdir -p /workspace/node_modules && echo junk > /workspace/node_modules/x && echo node_modules > /workspace/.gitignore")
	if err := v.Stop(ctx, name); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(rec.Worktree, "draft.txt")); string(b) != "draft\n" {
		t.Errorf("uncommitted work not saved: %q", b)
	}
	if _, err := os.Stat(filepath.Join(rec.Worktree, "node_modules")); err == nil {
		t.Error("ignored files were copied back")
	}
	if cs, _ := v.List(ctx, ""); len(cs) != 1 || cs[0].Running() {
		t.Errorf("after stop: %+v", cs)
	}
	if err := v.Remove(ctx, name); !IsKept(err) {
		t.Errorf("Remove of a dirty worktree = %v, want KeptError", err)
	}
	if cs, _ := v.List(ctx, ""); len(cs) != 0 {
		t.Errorf("after remove: %+v", cs)
	}
	if msg := sh(repo, "git log -1 --format=%s persona/t1"); msg != "vm commit" {
		t.Errorf("host branch lost: %q", msg)
	}
	sh(repo, "git worktree remove --force "+rec.Worktree)
}

func TestMain(m *testing.M) {
	if os.Getenv("PERSONA_TEST_RSH") != "" {
		args := os.Args[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-test.") {
			args = args[1:]
		}
		os.Exit(RshMain(args))
	}
	code := m.Run()
	if os.Getenv("PERSONA_FC_TEST") != "" {
		// Other packages' tests may run VMs concurrently; only ours count.
		out, _ := exec.Command("sudo", "-n", microvm.Wrapper, "list").Output()
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.Contains(line, `"id":"persona-tester-`) {
				fmt.Fprintf(os.Stderr, "leaked VM: %s\n", line)
				code = 1
			}
		}
	}
	os.Exit(code)
}
