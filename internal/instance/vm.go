//go:build linux

package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/viper"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/microvm"
	"github.com/johndavisdev/persona/internal/persona"
)

// VM runs instances as Firecracker microVMs. Images are still built with
// Docker, then flattened into a read-only root disk shared by every instance
// of that image. Each instance gets its own git worktree and branch on the
// host, a copy of it in the VM, and pushes only through the host broker.
type VM struct {
	Docker  string // docker binary, for images
	Kernel  string
	VCPUs   int64
	MemMiB  int64
	DiskGiB int
	// Agent is the persona-agent binary put into images; default: this
	// executable, which contains the agent (see internal/agent/guest).
	Agent string
	// Self is the persona executable, used for the rsync transport, attach,
	// and starting the broker (skipped when empty). Default: this executable.
	Self string
	// Rsh overrides rsync's remote shell (tests); it is passed VSOCK USER.
	Rsh string
}

// NewVM returns the microVM runtime configured by viper.
func NewVM() *VM {
	self, _ := os.Executable()
	return &VM{Docker: viper.GetString(config.KeyDockerBin), Kernel: viper.GetString(config.KeyVMKernel),
		VCPUs: viper.GetInt64(config.KeyVMVCPUs), MemMiB: viper.GetInt64(config.KeyVMMemory),
		DiskGiB: viper.GetInt(config.KeyVMDisk), Agent: self, Self: self}
}

func (v *VM) Name() string { return "firecracker" }

func (v *VM) Available(ctx context.Context) error {
	if err := docker.Available(ctx); err != nil {
		return fmt.Errorf("images are built with Docker: %w", err)
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		return errors.New("microVM workspaces are copied with rsync: install it on the host")
	}
	return microvm.Available(ctx, v.Kernel)
}

// record is what persona remembers about a microVM instance.
type record struct {
	Name     string    `json:"name"`
	Persona  string    `json:"persona"`
	Instance string    `json:"instance"`
	VMID     string    `json:"vm_id"`
	IP       string    `json:"ip"`
	Image    string    `json:"image"`
	Created  time.Time `json:"created"`
	// From the image config: who and how the session runs.
	User string   `json:"user"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"` // guest directory Claude Code starts in
	// Git workspaces: the host repo, this instance's worktree and branch, and
	// the branch pull requests target.
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Base     string `json:"base,omitempty"`
	// Plain directories are copied in and back out on stop.
	HostDir string `json:"host_dir,omitempty"`
}

const guestWorkspace = "/workspace"

var vmIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// vmID shortens an instance name to persona-vm's 48-character id limit.
func vmID(name string) string {
	if vmIDRe.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	short := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(short) > 39 {
		short = short[:39]
	}
	return strings.Trim(short, "-") + "-" + hex.EncodeToString(sum[:4])
}

func recordPath(name string) string { return filepath.Join(config.InstancesDir(), name+".json") }

func loadRecord(name string) (*record, error) {
	b, err := os.ReadFile(recordPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrNoMatch, name)
		}
		return nil, err
	}
	var r record
	return &r, json.Unmarshal(b, &r)
}

func (r *record) save() error {
	if err := os.MkdirAll(config.InstancesDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	tmp := recordPath(r.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, recordPath(r.Name))
}

func allRecords() ([]*record, error) {
	paths, err := filepath.Glob(filepath.Join(config.InstancesDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*record
	for _, p := range paths {
		if r, err := loadRecord(strings.TrimSuffix(filepath.Base(p), ".json")); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// lockInstances serializes record creation (names and IP addresses).
func lockInstances() (func(), error) {
	if err := os.MkdirAll(config.InstancesDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(config.InstancesDir(), ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// allocIP picks a free address; 10.200.0.0/24 is left for tests and tooling.
func allocIP(recs []*record) (net.IP, error) {
	used := map[string]bool{}
	for _, r := range recs {
		used[r.IP] = true
	}
	for n := 256; n < 65535; n++ {
		if n%256 == 0 || n%256 == 255 {
			continue
		}
		ip := net.IPv4(10, 200, byte(n>>8), byte(n))
		if !used[ip.String()] {
			return ip, nil
		}
	}
	return nil, errors.New("no free microVM addresses")
}

func (v *VM) List(ctx context.Context, personaName string) ([]Container, error) {
	recs, err := allRecords()
	if err != nil {
		return nil, err
	}
	running, err := microvm.Running(ctx)
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, r := range recs {
		if personaName != "" && r.Persona != personaName {
			continue
		}
		c := Container{ID: r.VMID, Name: r.Name, Persona: r.Persona, Instance: r.Instance, Image: r.Image,
			Created: r.Created.Format(time.RFC3339), State: "exited", Status: "Stopped"}
		if running[r.VMID] {
			c.State = "running"
			c.Status = fmt.Sprintf("Up %s · microVM %s", humanDuration(time.Since(r.Created)), r.IP)
		}
		if r.Branch != "" {
			c.Status += " · " + r.Branch
		}
		cs = append(cs, c)
	}
	sortContainers(cs)
	return cs, nil
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// imageConfig is the part of a Docker image's config a VM session honours.
type imageConfig struct {
	Env        []string
	User       string
	WorkingDir string
	Entrypoint []string
	Cmd        []string
}

func (v *VM) inspect(ctx context.Context, image string) (string, imageConfig, error) {
	out, err := exec.CommandContext(ctx, v.Docker, "image", "inspect", "-f", "{{.Id}} {{json .Config}}", image).Output()
	if err != nil {
		return "", imageConfig{}, fmt.Errorf("docker image inspect %s: %w", image, err)
	}
	id, js, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	var cfg imageConfig
	return id, cfg, json.Unmarshal([]byte(js), &cfg)
}

// rootfs returns the read-only root disk for an image, exporting it once per
// image and agent build.
func (v *VM) rootfs(ctx context.Context, personaName, image, imageID string, log io.Writer) (string, error) {
	agentBin, err := os.ReadFile(v.Agent)
	if err != nil {
		return "", fmt.Errorf("read persona-agent: %w", err)
	}
	sum := sha256.Sum256(agentBin)
	dir := filepath.Join(config.VMDir(), "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	_ = os.Chmod(config.VMDir(), 0o755)
	_ = os.Chmod(dir, 0o755)
	prefix := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(personaName, "-")
	id := strings.TrimPrefix(imageID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.ext4", prefix, id, hex.EncodeToString(sum[:4])))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	fmt.Fprintf(log, "Exporting %s to a microVM disk…\n", image)
	tmp := path + ".partial"
	if err := microvm.ExportRootfs(ctx, v.Docker, image, tmp,
		map[string]microvm.File{microvm.AgentPath: {Data: agentBin, Mode: 0o755}}); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	// Older exports of this persona are unused by new instances. Running ones
	// keep their hard link inside the jail, so removing these is safe.
	old, _ := filepath.Glob(filepath.Join(dir, prefix+"-*.ext4"))
	for _, o := range old {
		if o != path && !strings.HasSuffix(o, ".partial") {
			os.Remove(o)
		}
	}
	return path, nil
}

// overlay returns the blank writable-layer template persona-vm copies per VM.
func (v *VM) overlay(ctx context.Context) (string, error) {
	path := filepath.Join(config.VMDir(), fmt.Sprintf("overlay-%dg.ext4", v.DiskGiB))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(config.VMDir(), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".partial"
	if err := microvm.NewOverlay(ctx, tmp, v.DiskGiB); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func (v *VM) Run(ctx context.Context, o RunOptions) (name string, err error) {
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	image := o.Image
	if image == "" {
		image = persona.ImageFor(o.Persona)
	}
	imageID, cfg, err := v.inspect(ctx, image)
	if err != nil {
		return "", err
	}
	rootfs, err := v.rootfs(ctx, o.Persona, image, imageID, log)
	if err != nil {
		return "", err
	}
	overlay, err := v.overlay(ctx)
	if err != nil {
		return "", err
	}

	name = docker.ContainerName(o.Persona, o.Instance)
	unlock, err := lockInstances()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(recordPath(name)); err == nil {
		unlock()
		return "", fmt.Errorf("instance %s already exists (stop it first)", name)
	}
	recs, _ := allRecords()
	ip, err := allocIP(recs)
	if err != nil {
		unlock()
		return "", err
	}
	user := strings.SplitN(cfg.User, ":", 2)[0]
	if user == "" {
		user = "root"
	}
	rec := &record{Name: name, Persona: o.Persona, Instance: o.Instance, VMID: vmID(name), IP: ip.String(),
		Image: image, Created: time.Now(), User: user, Env: cfg.Env, Cwd: guestWorkspace}
	err = rec.save()
	unlock()
	if err != nil {
		return "", err
	}
	var vm *microvm.VM
	defer func() {
		if err != nil {
			if vm != nil {
				vm.Stop()
			}
			v.discard(ctx, rec)
		}
	}()

	src, cleanup, err := prepareWorkspace(ctx, rec, o.Workdir, log)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := rec.save(); err != nil {
		return "", err
	}

	vm, err = microvm.Start(ctx, microvm.Spec{ID: rec.VMID, Kernel: v.Kernel, Rootfs: rootfs, Overlay: overlay,
		VCPUs: v.VCPUs, MemMiB: v.MemMiB, IP: ip})
	if err != nil {
		return "", err
	}
	if src != "" {
		if _, err := vm.Agent.Run(ctx, agent.Exec{Argv: []string{"/bin/sh", "-c", "command -v rsync"},
			User: rec.User, Env: rec.Env, Dir: "/"}); err != nil {
			return "", fmt.Errorf("the %s image has no rsync; rebuild it (persona build %s) so workspaces can be copied in (%v)", image, o.Persona, err)
		}
		if err := v.rsync(ctx, rec, vm, src+"/", "vm:"+guestWorkspace+"/"); err != nil {
			return "", fmt.Errorf("copy workspace in: %w", err)
		}
	}
	if err := v.configureGuest(ctx, rec, vm, o); err != nil {
		return "", err
	}
	argv := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	if len(argv) == 0 {
		return "", fmt.Errorf("image %s has no entrypoint", image)
	}
	if _, err := vm.Agent.Run(ctx, agent.Exec{User: rec.User, Env: rec.Env, Dir: rec.Cwd, Detach: true,
		Argv: append([]string{"/bin/sh", "-c", `. /run/persona/env && exec "$@"`, "sh"}, argv...)}); err != nil {
		return "", fmt.Errorf("start session: %w", err)
	}
	if rec.Repo != "" && v.Self != "" {
		if err := EnsureBroker(v.Self); err != nil {
			fmt.Fprintf(log, "warning: push broker not started: %v\n", err)
		}
	}
	return name, nil
}

// configureGuest delivers secrets and per-instance settings.
func (v *VM) configureGuest(ctx context.Context, rec *record, vm *microvm.VM, o RunOptions) error {
	var env strings.Builder
	set := func(k, val string) { fmt.Fprintf(&env, "export %s=%s\n", k, shellQuote(val)) }
	for _, e := range o.Env {
		if k, val, ok := strings.Cut(e, "="); ok {
			set(k, val)
		} else if val, ok := os.LookupEnv(e); ok {
			set(e, val)
		}
	}
	set("PERSONA_NAME", rec.Persona)
	set("PERSONA_INSTANCE", rec.Name)
	if o.ClaudeArgs != "" {
		set("PERSONA_CLAUDE_ARGS", o.ClaudeArgs)
	}
	if o.Prompt != "" {
		set("PERSONA_PROMPT", o.Prompt)
	}
	if o.Cols > 0 && o.Rows > 0 {
		set("PERSONA_COLS", fmt.Sprint(o.Cols))
		set("PERSONA_ROWS", fmt.Sprint(o.Rows))
	}
	// /run is tmpfs: secrets never reach the VM's disk (or a snapshot of it).
	if err := vm.Agent.WriteFile(ctx, agent.Write{Path: "/run/persona/env", Mode: 0o600, User: rec.User,
		Data: []byte(env.String())}); err != nil {
		return err
	}
	if rec.Repo != "" {
		for _, k := range []string{"user.name", "user.email"} {
			if out, err := exec.CommandContext(ctx, "git", "-C", rec.Repo, "config", k).Output(); err == nil {
				if val := strings.TrimSpace(string(out)); val != "" {
					if _, err := v.guestArgs(ctx, rec, vm, "git", "config", "--global", k, val); err != nil {
						return err
					}
				}
			}
		}
		note := fmt.Sprintf(vmClaudeMD, rec.Branch, rec.Branch, rec.Base)
		if _, err := vm.Agent.Run(ctx, agent.Exec{User: rec.User, Env: rec.Env, Stdin: []byte(note),
			Argv: []string{"/bin/sh", "-c", `mkdir -p "$HOME/.claude" && cat >> "$HOME/.claude/CLAUDE.md"`}}); err != nil {
			return err
		}
	}
	return nil
}

const vmClaudeMD = `

## This instance's workspace

/workspace is a git checkout of branch ` + "`%s`" + `, made for this session only. Commit
your work there. This VM has no GitHub credentials, so ` + "`git push`" + ` will fail; instead:

- ` + "`persona-agent push`" + ` pushes ` + "`%s`" + ` to GitHub using the host's credentials.
- ` + "`persona-agent pr --title \"...\" --body \"...\"`" + ` pushes and opens a pull request against ` + "`%s`" + `.

Only this branch can be pushed, and only committed work is pushed.
`

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// guest runs a shell command as the session user.
func (v *VM) guest(ctx context.Context, rec *record, vm *microvm.VM, cmd string) ([]byte, error) {
	return v.guestArgs(ctx, rec, vm, "/bin/sh", "-c", cmd)
}

func (v *VM) guestArgs(ctx context.Context, rec *record, vm *microvm.VM, argv ...string) ([]byte, error) {
	resp, err := vm.Agent.Run(ctx, agent.Exec{Argv: argv, User: rec.User, Env: rec.Env, Dir: rec.Cwd})
	return resp.Stdout, err
}

// rsync copies between the host and the guest (written "vm:/path") over the
// agent's stream transport, as the session user.
func (v *VM) rsync(ctx context.Context, rec *record, vm *microvm.VM, args ...string) error {
	rsh := v.Rsh
	if rsh == "" {
		rsh = shellQuote(v.Self) + " __vm-rsh"
	}
	rsh += " " + shellQuote(vm.Vsock) + " " + shellQuote(rec.User)
	cmd := exec.CommandContext(ctx, "rsync", append([]string{"-rlptD", "--no-owner", "--no-group", "-e", rsh}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// RshMain is rsync's remote shell for VMs: VSOCK USER HOST COMMAND... It
// runs COMMAND in the guest with stdin/stdout connected to this process.
func RshMain(args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: __vm-rsh VSOCK USER HOST COMMAND...")
		return 2
	}
	conn, err := agent.Client{Vsock: args[0]}.Stream(context.Background(), agent.Exec{Argv: args[3:], User: args[1]})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go io.Copy(conn, os.Stdin)
	io.Copy(os.Stdout, conn)
	return 0
}

func (v *VM) lookup(name string) (*record, *microvm.VM, error) {
	rec, err := loadRecord(name)
	if err != nil {
		return nil, nil, err
	}
	vm, ok := microvm.Lookup(rec.VMID)
	if !ok {
		return rec, nil, fmt.Errorf("%s is not running", name)
	}
	vm.IP = net.ParseIP(rec.IP)
	return rec, vm, nil
}

func (v *VM) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	rec, vm, err := v.lookup(name)
	if err != nil {
		return nil, err
	}
	return v.guestArgs(ctx, rec, vm, args...)
}

func (v *VM) AttachCommand(ctx context.Context, name string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, v.Self, "__vm-attach", name)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// Stop saves the session's work to the host and stops the VM: commits land
// on the instance's branch (fast-forwarding its worktree), and uncommitted
// changes are copied into the worktree, so nothing is lost. The record stays
// until Remove.
func (v *VM) Stop(ctx context.Context, name string) error {
	rec, vm, err := v.lookup(name)
	if err != nil {
		if rec != nil {
			return nil // already stopped
		}
		return err
	}
	if !forced(ctx) {
		if err := v.saveWork(ctx, rec, vm); err != nil {
			return fmt.Errorf("%s: saving work failed, VM left running (--force stops it anyway, losing that work): %w", name, err)
		}
	}
	return microvm.Stop(ctx, rec.VMID)
}

func (v *VM) saveWork(ctx context.Context, rec *record, vm *microvm.VM) error {
	switch {
	case rec.Worktree != "":
		if _, err := fetchBranch(ctx, rec, vm); err != nil {
			return err
		}
		return v.rsync(ctx, rec, vm, "--delete", "--exclude=/.git", "--filter=:- .gitignore",
			"vm:"+guestWorkspace+"/", rec.Worktree+"/")
	case rec.HostDir != "":
		return v.rsync(ctx, rec, vm, "--exclude=/.git", "--filter=:- .gitignore", "vm:"+guestWorkspace+"/", rec.HostDir+"/")
	}
	return nil
}

// Remove deletes the record and the worktree. The branch is always kept; a
// worktree with uncommitted changes is kept too (reported as a KeptError).
func (v *VM) Remove(ctx context.Context, name string) error {
	rec, err := loadRecord(name)
	if err != nil {
		return err
	}
	if _, running := microvm.Lookup(rec.VMID); running {
		if err := v.Stop(ctx, name); err != nil {
			return err
		}
	}
	return v.discard(ctx, rec)
}

func (v *VM) discard(ctx context.Context, rec *record) error {
	var kept error
	if rec.Worktree != "" {
		if _, err := os.Stat(rec.Worktree); err == nil {
			if out, err := exec.CommandContext(ctx, "git", "-C", rec.Repo, "worktree", "remove", rec.Worktree).CombinedOutput(); err != nil {
				kept = &KeptError{Msg: fmt.Sprintf("kept worktree %s (branch %s): %s", rec.Worktree, rec.Branch,
					strings.TrimSpace(string(out)))}
			}
		}
	}
	if err := os.Remove(recordPath(rec.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return kept
}
