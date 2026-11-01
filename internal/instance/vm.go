//go:build linux

package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
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
	"github.com/johndavisdev/persona/internal/status"
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
	Started  time.Time `json:"started,omitempty"` // last (re)start
	// From the image config: who and how the session runs. Kept so the spawn
	// can restart, or be saved, after its persona is deleted or rebuilt.
	User       string   `json:"user"`
	Env        []string `json:"env"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cwd        string   `json:"cwd"` // guest directory Claude Code starts in
	ClaudeArgs string   `json:"claude_args,omitempty"`
	// Disks (in DisksDir): a hard link to the image's root disk, shared and
	// read-only, and the spawn's writable disk while it is stopped.
	Rootfs string `json:"rootfs,omitempty"`
	Disk   string `json:"disk,omitempty"`
	// Stores are hard links to the image's /nix/store layers (see microvm.Store).
	Stores []string `json:"stores,omitempty"`
	// Git workspaces: the host repo, this instance's worktree and branch, and
	// the branch pull requests target.
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Base     string `json:"base,omitempty"`
	// Plain directories are copied in and back out on stop.
	HostDir string `json:"host_dir,omitempty"`
	// Namespace is the persona network namespace ("" is DefaultNamespace).
	Namespace string `json:"namespace,omitempty"`
}

func (r *record) namespace() string {
	if r.Namespace == "" {
		return DefaultNamespace
	}
	return r.Namespace
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

// disksDir holds a spawn's disks while it exists.
func disksDir(name string) string { return filepath.Join(config.InstancesDir(), name+".disks") }

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
			Created: r.Created.Format(time.RFC3339), State: "exited", Status: "Stopped", Namespace: r.namespace(), IP: r.IP}
		if running[r.VMID] {
			c.State = "running"
			since := r.Started
			if since.IsZero() {
				since = r.Created
			}
			c.Status = fmt.Sprintf("Up %s · microVM %s", humanDuration(time.Since(since)), r.IP)
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
	Labels     map[string]string
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
func (v *VM) rootfs(ctx context.Context, personaName, image, imageID string, cfg imageConfig, log io.Writer) (string, []string, error) {
	agentBin, err := os.ReadFile(v.Agent)
	if err != nil {
		return "", nil, fmt.Errorf("read persona-agent: %w", err)
	}
	// The kernel runs it before anything is mounted, /nix/store included.
	if f, err := elf.NewFile(bytes.NewReader(agentBin)); err == nil {
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				return "", nil, fmt.Errorf("%s is dynamically linked; persona-agent runs as the microVM's init and must be static (build with CGO_ENABLED=0)", v.Agent)
			}
		}
	}
	extra := map[string]microvm.File{microvm.AgentPath: {Data: agentBin, Mode: 0o755}}
	edits := map[string]microvm.Edit{}
	h := sha256.New()
	h.Write(agentBin)
	// An image whose /nix/store is just what Nix built shares it through the
	// store (see microvm.Store); one changed by hand (persona customize) has
	// its own copy.
	split := cfg.Labels[persona.LabelStore] != persona.StorePrivate
	if split {
		h.Write([]byte("store-v1"))
	}
	sum := h.Sum(nil)
	dir := filepath.Join(config.VMDir(), "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
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
		layers, err := readLayers(path)
		if err == nil {
			return path, storeDisks(layers), nil
		}
		// A disk without its layer list is incomplete: export it again.
	}
	fmt.Fprintf(log, "Exporting %s to a microVM disk…\n", image)
	tmp := path + ".partial"
	var layers []string
	if split {
		layers, err = microvm.ExportSplit(ctx, v.Docker, image, tmp, extra, edits, store())
	} else {
		err = microvm.ExportRootfs(ctx, v.Docker, image, tmp, extra, edits)
	}
	if err != nil {
		return "", nil, err
	}
	if err := writeLayers(path, layers); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", nil, err
	}
	// Older exports of this persona are unused by new instances. Running ones
	// keep their hard links inside the jail, so removing these is safe.
	for _, o := range imageDisks(prefix) {
		if o != path {
			removeDisk(o)
		}
	}
	gcStore()
	return path, storeDisks(layers), nil
}

// store is the shared /nix/store of every microVM disk.
func store() microvm.Store { return microvm.Store{Dir: filepath.Join(config.VMDir(), "store")} }

func storeDisks(layers []string) []string {
	var out []string
	for _, l := range layers {
		out = append(out, store().Disk(l))
	}
	return out
}

// An image disk's store layers are listed next to it, in <disk>.layers.
func layersPath(disk string) string { return strings.TrimSuffix(disk, ".ext4") + ".layers" }

func readLayers(disk string) ([]string, error) {
	b, err := os.ReadFile(layersPath(disk))
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(disk); statErr == nil {
			return nil, nil // exported before the store: everything is in the disk
		}
	}
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(b)), nil
}

func writeLayers(disk string, layers []string) error {
	return os.WriteFile(layersPath(disk), []byte(strings.Join(layers, "\n")+"\n"), 0o644)
}

func removeDisk(disk string) {
	os.Remove(disk)
	os.Remove(layersPath(disk))
}

// gcStore deletes store layers no image disk uses. Spawns hold their own
// hard links, so their layers live on until they are killed.
func gcStore() {
	inUse := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(config.VMDir(), "images"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".layers") {
			b, _ := os.ReadFile(filepath.Join(config.VMDir(), "images", e.Name()))
			for _, l := range strings.Fields(string(b)) {
				inUse[l] = true
			}
		}
	}
	_ = store().GC(inUse)
}

// Prepare exports image's microVM disk and creates the blank overlay disk,
// unless they already exist. Spawn does the same on a cache miss.
func (v *VM) Prepare(ctx context.Context, personaName, image string) error {
	imageID, cfg, err := v.inspect(ctx, image)
	if err != nil {
		return err
	}
	if _, _, err := v.rootfs(ctx, personaName, image, imageID, cfg, io.Discard); err != nil {
		return err
	}
	_, err = v.overlay(ctx)
	return err
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
	rootfs, stores, err := v.rootfs(ctx, o.Persona, image, imageID, cfg, log)
	if err != nil {
		return "", err
	}
	overlay, err := v.overlay(ctx)
	if err != nil {
		return "", err
	}

	ns := o.Namespace
	if ns == DefaultNamespace {
		ns = ""
	}
	if ns != "" {
		if err := v.checkNamespace(ctx, ns); err != nil {
			return "", err
		}
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
		Image: image, Created: time.Now(), Started: time.Now(), User: user, Env: cfg.Env, Cwd: guestWorkspace, ClaudeArgs: o.ClaudeArgs,
		Entrypoint: append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...), Namespace: ns}
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

	// The spawn keeps its own hard link to the root disk, so rebuilding or
	// deleting the persona never affects it.
	if rec.Rootfs, err = linkDisk(rootfs, rec.Name); err != nil {
		return "", err
	}
	if rec.Stores, err = linkStores(stores, rec.Name); err != nil {
		return "", err
	}
	src, cleanup, err := prepareWorkspace(ctx, rec, o.Workdir, log)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := rec.save(); err != nil {
		return "", err
	}

	vm, err = microvm.Start(ctx, microvm.Spec{ID: rec.VMID, Kernel: v.Kernel, Rootfs: rec.Rootfs, Stores: rec.Stores, Overlay: overlay,
		VCPUs: v.VCPUs, MemMiB: v.MemMiB, IP: ip, Namespace: rec.Namespace, Spawn: rec.Instance, Persona: rec.Persona})
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
	if err := v.startSession(ctx, rec, vm, log); err != nil {
		return "", err
	}
	return name, nil
}

// startSession launches the image's entrypoint (Claude Code in tmux).
func (v *VM) startSession(ctx context.Context, rec *record, vm *microvm.VM, log io.Writer) error {
	if len(rec.Entrypoint) == 0 {
		return fmt.Errorf("image %s has no entrypoint", rec.Image)
	}
	if _, err := vm.Agent.Run(ctx, agent.Exec{User: rec.User, Env: rec.Env, Dir: rec.Cwd, Detach: true,
		Argv: append([]string{"/bin/sh", "-c", `. /run/persona/env && exec "$@"`, "sh"}, rec.Entrypoint...)}); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if v.Self != "" { // status events for every spawn; push/pr for git ones
		if err := EnsureBroker(v.Self); err != nil {
			fmt.Fprintf(log, "warning: push broker not started: %v\n", err)
		}
	}
	return nil
}

// linkDisk hard links a shared root disk into a spawn's disks directory
// (copying it if the filesystems differ).
// linkStores gives a spawn its own hard links to its store layers.
func linkStores(srcs []string, name string) ([]string, error) {
	var out []string
	for _, src := range srcs {
		dir := disksDir(name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		dst := filepath.Join(dir, "store-"+filepath.Base(src))
		_ = os.Remove(dst)
		if err := os.Link(src, dst); err != nil {
			dst = src // e.g. across filesystems: fall back to the shared path
		}
		out = append(out, dst)
	}
	return out, nil
}

func linkDisk(src, name string) (string, error) {
	dir := disksDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "rootfs.ext4")
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err != nil {
		return src, nil // e.g. across filesystems: fall back to the shared path
	}
	return dst, nil
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
		// Claude Code reads ~/.claude/CLAUDE.md; pi reads ~/.pi/agent/AGENTS.md.
		if _, err := vm.Agent.Run(ctx, agent.Exec{User: rec.User, Env: rec.Env, Stdin: []byte(note),
			Argv: []string{"/bin/sh", "-c", `f="$HOME/.claude/CLAUDE.md"; [ "${PERSONA_HARNESS:-}" = pi ] && f="$HOME/.pi/agent/AGENTS.md"; mkdir -p "$(dirname "$f")"; grep -qF "## This instance's workspace" "$f" 2>/dev/null || cat >> "$f"`}}); err != nil {
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
- ` + "`persona-agent fetch [branch...]`" + ` brings the host's branches in as ` + "`host/<branch>`" + `
  (default: the pull request base), e.g. before ` + "`git rebase host/main`" + `. ` + "`git fetch`" + ` won't work.

Only this branch can be pushed, and only committed work is pushed. The persona:git skill has the details.
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
	argv := args[3:]
	// git (like ssh) passes the remote command as one string to be run by a
	// shell: "git-receive-pack '/workspace'". rsync passes separate arguments.
	if len(argv) == 1 && strings.ContainsAny(argv[0], " '") {
		argv = []string{"/bin/sh", "-c", argv[0]}
	}
	conn, err := agent.Client{Vsock: args[0]}.Stream(context.Background(), agent.Exec{Argv: argv, User: args[1]})
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

// Stop saves the session's work to the host and stops the VM, keeping the
// spawn so Start can resume it: commits land on its branch (fast-forwarding
// the worktree) and uncommitted changes are copied into the worktree, then the
// VM's writable disk is copied out of its jail.
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
		if err := vm.Agent.Quiesce(ctx); err != nil {
			return fmt.Errorf("%s: flushing disks: %w", name, err)
		}
		dir := disksDir(rec.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := vm.SaveDisk(ctx, dir); err != nil {
			return fmt.Errorf("%s: saving its disk failed, VM left paused: %w", name, err)
		}
		rec.Disk = filepath.Join(dir, microvm.DiskName)
		if err := rec.save(); err != nil {
			return err
		}
	}
	return microvm.Stop(ctx, rec.VMID)
}

// Start resumes a stopped spawn from its disks: same name, address, branch
// and files, with a fresh Claude Code session and current credentials.
func (v *VM) Start(ctx context.Context, name string, env []string) error {
	rec, err := loadRecord(name)
	if err != nil {
		return err
	}
	if _, running := microvm.Lookup(rec.VMID); running {
		return nil
	}
	if len(rec.Entrypoint) == 0 || rec.Rootfs == "" {
		// Records from before spawns were restartable: use the persona's image.
		imageID, cfg, err := v.inspect(ctx, rec.Image)
		if err != nil {
			return fmt.Errorf("%s: its image is gone (%w); kill it and spawn again", name, err)
		}
		rec.Entrypoint = append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
		rootfs, stores, err := v.rootfs(ctx, rec.Persona, rec.Image, imageID, cfg, io.Discard)
		if err != nil {
			return err
		}
		if rec.Rootfs, err = linkDisk(rootfs, rec.Name); err != nil {
			return err
		}
		if rec.Stores, err = linkStores(stores, rec.Name); err != nil {
			return err
		}
	}
	disk := rec.Disk
	if _, err := os.Stat(disk); disk == "" || err != nil {
		if disk, err = v.overlay(ctx); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "warning: %s has no saved disk (it was stopped before spawns kept one, or with --force); it starts from a clean disk\n", name)
	}
	rec.Started = time.Now()
	if err := rec.save(); err != nil {
		return err
	}
	vm, err := microvm.Start(ctx, microvm.Spec{ID: rec.VMID, Kernel: v.Kernel, Rootfs: rec.Rootfs, Stores: rec.Stores, Overlay: disk,
		VCPUs: v.VCPUs, MemMiB: v.MemMiB, IP: net.ParseIP(rec.IP), Namespace: rec.Namespace, Spawn: rec.Instance, Persona: rec.Persona})
	if err != nil {
		return err
	}
	if err := v.configureGuest(ctx, rec, vm, RunOptions{Env: env, ClaudeArgs: rec.ClaudeArgs}); err == nil {
		err = v.startSession(ctx, rec, vm, io.Discard)
	}
	if err != nil {
		vm.Stop()
		return err
	}
	return nil
}

// Save captures a running spawn's filesystem (without /workspace, which every
// spawn refills, or runtime mounts such as /proc and /run) as the image for
// persona newPersona, then prepares its microVM disk. The spawn keeps running.
func (v *VM) Save(ctx context.Context, name, newPersona string) error {
	rec, vm, err := v.lookup(name)
	if err != nil {
		if rec != nil {
			return fmt.Errorf("%w; start it first (`persona start %s`)", err, rec.Instance)
		}
		return err
	}
	if len(rec.Entrypoint) == 0 {
		if _, cfg, err := v.inspect(ctx, rec.Image); err == nil {
			rec.Entrypoint = append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
		}
	}
	conn, err := vm.Agent.Stream(ctx, agent.Exec{User: "root", Dir: "/", Argv: []string{"tar",
		"--create", "--file=-", "--one-file-system", "--numeric-owner", "--xattrs", "--acls",
		"--exclude=./workspace/*", "--exclude=./.oldroot", "--exclude=./etc/persona/nix-fallback", "."}})
	if err != nil {
		return err
	}
	defer conn.Close()
	image := persona.ImageFor(newPersona)
	if err := docker.Import(ctx, conn, image, imageChanges(rec, newPersona)); err != nil {
		return err
	}
	return v.Prepare(ctx, newPersona, image)
}

// imageChanges recreate a spawn's image config on a saved image.
func imageChanges(rec *record, newPersona string) []string {
	changes := []string{
		"USER " + rec.User,
		"WORKDIR " + guestWorkspace,
		fmt.Sprintf("LABEL %s=%q %s=%q", persona.LabelManaged, "true", persona.LabelName, newPersona),
	}
	for _, kv := range rec.Env {
		if k, val, ok := strings.Cut(kv, "="); ok {
			changes = append(changes, fmt.Sprintf("ENV %s=%q", k, val))
		}
	}
	if len(rec.Entrypoint) > 0 {
		ep, _ := json.Marshal(rec.Entrypoint)
		changes = append(changes, "ENTRYPOINT "+string(ep))
	}
	return changes
}

func (v *VM) saveWork(ctx context.Context, rec *record, vm *microvm.VM) error {
	switch {
	case rec.Worktree != "":
		if _, err := fetchBranch(ctx, rec, vm, fastForward); err != nil {
			// The spawn rewrote its branch (a rebase): its history wins, as
			// its files are about to. The old head stays in the reflog.
			old, _ := git(ctx, rec.Worktree, "rev-parse", "--short", "HEAD")
			if _, ferr := fetchBranch(ctx, rec, vm, replaceAll); ferr != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "note: %s: the spawn rewrote %s; the worktree now has its history (was %s; see git reflog)\n",
				rec.Instance, rec.Branch, old)
		}
		return v.rsync(ctx, rec, vm, "--delete", "--exclude=/.git", "--filter=:- .gitignore",
			"vm:"+guestWorkspace+"/", rec.Worktree+"/")
	case rec.HostDir != "":
		return v.rsync(ctx, rec, vm, "--exclude=/.git", "--filter=:- .gitignore", "vm:"+guestWorkspace+"/", rec.HostDir+"/")
	}
	return nil
}

// Remove kills a spawn: a running one saves its work to the host first (as
// Stop does, without keeping its disk), then the VM, its disks, record and
// worktree go. The branch is always kept; a worktree with uncommitted changes
// is kept too (reported as a KeptError).
func (v *VM) Remove(ctx context.Context, name string) error {
	rec, err := loadRecord(name)
	if err != nil {
		return err
	}
	if vm, running := microvm.Lookup(rec.VMID); running {
		if !forced(ctx) {
			if err := v.saveWork(ctx, rec, vm); err != nil {
				return fmt.Errorf("%s: saving work failed, VM left running (--force kills it anyway, losing that work): %w", name, err)
			}
		}
		if err := microvm.Stop(ctx, rec.VMID); err != nil {
			return err
		}
	}
	if err := microvm.RemoveForwardsTo(ctx, rec.VMID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: removing its port forwards: %v\n", name, err)
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
	if err := os.RemoveAll(disksDir(rec.Name)); err != nil {
		return err
	}
	_ = status.Remove(rec.Instance)
	if err := os.Remove(recordPath(rec.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return kept
}

// imageDisks lists the cached microVM disks of one persona: exactly
// <persona>-<image id>-<agent hash>.ext4, so "app" never matches "app-v2".
func imageDisks(prefix string) []string {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-[0-9a-f]{12}-[0-9a-f]{8}\.ext4$`)
	entries, _ := os.ReadDir(filepath.Join(config.VMDir(), "images"))
	var out []string
	for _, e := range entries {
		if re.MatchString(e.Name()) {
			out = append(out, filepath.Join(config.VMDir(), "images", e.Name()))
		}
	}
	return out
}

// ForgetImages deletes the cached microVM disks of a persona's images.
// Spawns keep their own hard links, so they are unaffected.
func (v *VM) ForgetImages(personaName string) error {
	prefix := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(personaName, "-")
	for _, p := range imageDisks(prefix) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		os.Remove(layersPath(p))
	}
	gcStore()
	return nil
}
