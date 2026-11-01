//go:build linux

package microvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/johndavisdev/persona/internal/agent"
)

func TestValidate(t *testing.T) {
	base := Spec{ID: "a", Kernel: "k", Rootfs: "r", IP: net.IPv4(10, 200, 0, 10)}
	if err := base.validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	for name, ip := range map[string]net.IP{"gateway": Gateway, "off-subnet": net.IPv4(172, 16, 1, 5), "nil": nil} {
		s := base
		s.IP = ip
		if err := s.validate(); err == nil {
			t.Errorf("%s: expected error for IP %v", name, ip)
		}
	}
}

func TestKernelArgsAndIdentity(t *testing.T) {
	ip := net.IPv4(10, 200, 3, 4)
	if got := MAC(ip); got != "06:00:0a:c8:03:04" {
		t.Errorf("MAC = %s", got)
	}
	want := "init=/sbin/persona-agent ip=10.200.3.4::10.200.0.1:255.255.0.0:vm1:eth0:off:10.200.0.1:1.1.1.1" // persona-dns, then a public resolver
	if got := KernelArgs(ip, "vm1"); !strings.HasSuffix(got, want) {
		t.Errorf("KernelArgs = %s", got)
	}
	id := Identity("vm1", ip)
	if id.Addr != "10.200.3.4/16" || id.MAC != MAC(ip) || len(id.Seed) != 64 {
		t.Errorf("Identity = %+v", id)
	}
	if bytes.Equal(id.Seed, Identity("vm1", ip).Seed) {
		t.Error("identity seeds repeat")
	}
}

// The tests below boot real microVMs through persona-vm, unprivileged. They
// need `the persona package installed`, Docker, and the guest kernel:
//
//	PERSONA_FC_TEST=1 PERSONA_FC_ASSETS=$HOME/.config/persona/firecracker \
//	  PERSONA_FC_LAN_PROBE=172.16.1.1 go test ./internal/microvm -v
//
// The guest image is Debian with persona-agent as init, exported from Docker
// the same way `persona build` does it, and cached in PERSONA_FC_ASSETS.
type env struct {
	t       *testing.T
	ctx     context.Context
	kernel  string
	rootfs  string
	overlay string // blank template; persona-vm copies it per VM
	assets  string
}

const guestDockerfile = `FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      rsync git ca-certificates curl iputils-ping && rm -rf /var/lib/apt/lists/*
`

var (
	assetsOnce sync.Once
	assetsErr  error
	rootfsPath string
)

func newEnv(t *testing.T) *env {
	if os.Getenv("PERSONA_FC_TEST") == "" {
		t.Skip("set PERSONA_FC_TEST=1 (requires the persona package installed and Docker)")
	}
	assets := os.Getenv("PERSONA_FC_ASSETS")
	kernels, _ := filepath.Glob(filepath.Join(assets, "vmlinux-*"))
	if len(kernels) == 0 {
		t.Fatalf("no vmlinux-* in PERSONA_FC_ASSETS=%q", assets)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	assetsOnce.Do(func() { rootfsPath, assetsErr = buildGuest(ctx, assets) })
	if assetsErr != nil {
		t.Fatal(assetsErr)
	}
	overlay := filepath.Join(assets, "test-overlay.ext4")
	if _, err := os.Stat(overlay); err != nil {
		if err := NewOverlay(ctx, overlay, 8); err != nil {
			t.Fatal(err)
		}
	}
	return &env{t: t, ctx: ctx, kernel: kernels[len(kernels)-1], rootfs: rootfsPath, overlay: overlay, assets: assets}
}

// buildGuest builds the test image with Docker and exports it with the
// current persona-agent, reusing the result while neither changes.
func buildGuest(ctx context.Context, assets string) (string, error) {
	dir, err := os.MkdirTemp("", "persona-guest")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	agentBin := filepath.Join(dir, "persona-agent")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", agentBin, "github.com/johndavisdev/persona/cmd/persona-agent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build agent: %v: %s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(guestDockerfile), 0o644)
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", "persona-test-guest", dir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker build: %v: %s", err, out)
	}
	imgID, err := exec.CommandContext(ctx, "docker", "image", "inspect", "-f", "{{.Id}}", "persona-test-guest").Output()
	if err != nil {
		return "", err
	}
	agentData, err := os.ReadFile(agentBin)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(append(agentData, imgID...))
	out := filepath.Join(assets, "test-rootfs-"+hex.EncodeToString(h[:6])+".ext4")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	old, _ := filepath.Glob(filepath.Join(assets, "test-rootfs-*.ext4"))
	for _, o := range old {
		os.Remove(o)
	}
	return out, ExportRootfs(ctx, "docker", "persona-test-guest", out,
		map[string]File{AgentPath: {Data: agentData, Mode: 0o755}}, nil)
}

func (e *env) start(id string, ip net.IP) *VM {
	e.t.Helper()
	vm, err := Start(e.ctx, Spec{ID: id, Kernel: e.kernel, Rootfs: e.rootfs, Overlay: e.overlay,
		VCPUs: 1, MemMiB: 512, IP: ip})
	if err != nil {
		e.t.Fatal(err)
	}
	e.cleanup(vm)
	return vm
}

func (e *env) cleanup(vm *VM) {
	e.t.Cleanup(func() {
		if e.t.Failed() {
			e.t.Logf("%s console:\n%s", vm.ID, vm.ConsoleLog())
		}
		vm.Stop()
	})
}

// sh runs a shell command in the guest and returns its output.
func (e *env) sh(vm *VM, cmd string) (string, error) {
	resp, err := vm.Agent.Run(e.ctx, agent.Exec{Argv: []string{"/bin/sh", "-c", cmd}})
	return strings.TrimSpace(string(resp.Stdout) + string(resp.Stderr)), err
}

func (e *env) mustSh(vm *VM, cmd string) string {
	e.t.Helper()
	out, err := e.sh(vm, cmd)
	if err != nil {
		e.t.Fatalf("%s: %q: %v", vm.ID, cmd, err)
	}
	return out
}

// TestBootAndIsolation boots two VMs in the shared namespace and checks the
// overlay root, that they reach each other and the internet, and that they
// can't reach the host or the LAN.
func TestBootAndIsolation(t *testing.T) {
	e := newEnv(t)
	a, b := net.IPv4(10, 200, 0, 10), net.IPv4(10, 200, 0, 11)
	vmA := e.start("test-a", a)
	e.start("test-b", b)

	if out := e.mustSh(vmA, "grep ' / ' /proc/mounts"); !strings.HasPrefix(out, "overlay / overlay") {
		t.Errorf("root is not the overlay: %s", out)
	}
	e.mustSh(vmA, "echo ok > /etc/persona-written && cat /etc/persona-written")
	if out := e.mustSh(vmA, "cat /proc/1/comm"); out != "persona-agent" {
		t.Errorf("PID 1 is %q", out)
	}
	if out, err := exec.Command("ps", "-o", "user=", "-C", "firecracker").Output(); err == nil {
		for _, u := range strings.Fields(string(out)) {
			if u == "root" || u == os.Getenv("USER") {
				t.Errorf("firecracker runs as %s, want a jail uid", u)
			}
		}
	}

	expect := func(desc, cmd string, ok bool) {
		t.Helper()
		out, err := e.sh(vmA, cmd)
		if (err == nil) != ok {
			t.Errorf("%s: want ok=%v, got err=%v\n%s", desc, ok, err, out)
		} else {
			t.Logf("%s: ok=%v as expected", desc, ok)
		}
	}
	expect("VM to VM", "ping -c1 -W2 "+b.String(), true)
	expect("internet by IP", "ping -c1 -W3 1.1.1.1", true)
	expect("internet by DNS", "curl -sS -m8 -o /dev/null https://github.com", true)
	expect("host via veth", "ping -c1 -W2 10.199.0.1", false)
	if lan := os.Getenv("PERSONA_FC_LAN_PROBE"); lan != "" {
		expect("LAN host "+lan+" ping", "ping -c1 -W2 "+lan, false)
		expect("LAN host "+lan+" tcp/53", "timeout 3 bash -c '</dev/tcp/"+lan+"/53'", false)
	}
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		if n, ok := ad.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			expect("host address "+n.IP.String()+" tcp/22", "timeout 3 bash -c '</dev/tcp/"+n.IP.String()+"/22'", false)
		}
	}
}

// TestSnapshotClones snapshots a booted VM and restores two clones of it at
// once, each with its own address, MAC, hostname, clock and entropy.
func TestSnapshotClones(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now()
	tmpl, err := Start(e.ctx, Spec{ID: "test-tmpl", Kernel: e.kernel, Rootfs: e.rootfs, Overlay: e.overlay,
		VCPUs: 2, MemMiB: 1024, IP: net.IPv4(10, 200, 0, 20)})
	if err != nil {
		t.Fatal(err)
	}
	cold := time.Since(t0)
	e.mustSh(tmpl, "echo from-memory > /dev/shm/marker && echo from-disk > /root/marker")
	// Snapshot on the same filesystem as /srv/jailer (as persona's home is) so
	// restores hard link guest memory instead of copying it.
	dir, err := os.MkdirTemp(e.assets, "test-snapshot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	snap, err := tmpl.Snapshot(e.ctx, dir)
	tmpl.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, jailOverlay)); err == nil {
		t.Logf("snapshot overlay: %d MiB apparent, %d MiB on disk", fi.Size()>>20, du(fi)>>20)
	}
	// The snapshot's clock is now stale; clones must catch up.
	time.Sleep(2 * time.Second)

	ips := []net.IP{net.IPv4(10, 200, 0, 21), net.IPv4(10, 200, 0, 22)}
	clones := make([]*VM, len(ips))
	took := make([]time.Duration, len(ips))
	var wg sync.WaitGroup
	errs := make([]error, len(ips))
	for i, ip := range ips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t1 := time.Now()
			clones[i], errs[i] = Restore(e.ctx, snap, fmt.Sprintf("test-clone-%d", i), ip)
			took[i] = time.Since(t1)
		}()
	}
	wg.Wait()
	for i := range ips {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		e.cleanup(clones[i])
	}

	rand := map[string]bool{}
	for i, vm := range clones {
		if out := e.mustSh(vm, "cat /dev/shm/marker /root/marker"); out != "from-memory\nfrom-disk" {
			t.Errorf("%s lost state: %q", vm.ID, out)
		}
		if out := e.mustSh(vm, "hostname"); out != vm.ID {
			t.Errorf("hostname = %q, want %q", out, vm.ID)
		}
		if out := e.mustSh(vm, "cat /sys/class/net/eth0/address"); out != MAC(vm.IP) {
			t.Errorf("%s MAC = %s", vm.ID, out)
		}
		guest, _ := strconv.ParseInt(e.mustSh(vm, "date +%s"), 10, 64)
		if skew := time.Now().Unix() - guest; skew < -1 || skew > 1 {
			t.Errorf("%s clock off by %ds", vm.ID, skew)
		}
		rand[e.mustSh(vm, "head -c16 /dev/urandom | od -An -tx1")] = true
		peer := ips[(i+1)%len(ips)]
		if _, err := e.sh(vm, "ping -c1 -W2 "+peer.String()); err != nil {
			t.Errorf("%s can't reach sibling %v: %v", vm.ID, peer, err)
		}
		if _, err := e.sh(vm, "curl -sS -m8 -o /dev/null https://github.com"); err != nil {
			t.Errorf("%s has no internet: %v", vm.ID, err)
		}
	}
	if len(rand) != len(clones) {
		t.Error("clones produced identical random bytes")
	}
	t.Logf("cold boot to agent ready: %v", cold.Round(time.Millisecond))
	for i := range clones {
		t.Logf("clone %d: restored and identified in %v", i, took[i].Round(time.Millisecond))
		if took[i] > time.Second {
			t.Errorf("clone %d took %v, want < 1s", i, took[i])
		}
	}
}

// du is the space a file actually occupies on disk.
func du(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return 0
}

// TestStreams moves files with rsync and commits with git bundle, both over
// the agent's raw stream transport.
func TestStreams(t *testing.T) {
	e := newEnv(t)
	vm := e.start("test-streams", net.IPv4(10, 200, 0, 30))

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello\n"), 0o644)
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "b.bin"), bytes.Repeat([]byte{7}, 1<<20), 0o600)
	self, _ := os.Executable()
	rsh := self + " -test.run=^$"
	rsync := func(args ...string) {
		t.Helper()
		// Ownership is the guest's business (the receiver runs as root here).
		cmd := exec.CommandContext(e.ctx, "rsync", append([]string{"-a", "--no-owner", "--no-group", "-e", rsh}, args...)...)
		cmd.Env = append(os.Environ(), "PERSONA_TEST_STREAM="+vm.Vsock)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rsync %v: %v\n%s", args, err, out)
		}
	}
	rsync(src+"/", "vm:/workspace/")
	if out := e.mustSh(vm, "cat /workspace/a.txt; stat -c %a /workspace/sub/b.bin"); out != "hello\n600" {
		t.Errorf("guest sees %q", out)
	}
	e.mustSh(vm, "echo changed > /workspace/a.txt")
	back := t.TempDir()
	rsync("vm:/workspace/", back+"/")
	if b, _ := os.ReadFile(filepath.Join(back, "a.txt")); string(b) != "changed\n" {
		t.Errorf("rsync back: %q", b)
	}

	e.mustSh(vm, "cd /workspace && git init -q -b persona/test && git -c user.email=a@b -c user.name=a add -A && "+
		"git -c user.email=a@b -c user.name=a commit -qm 'from the vm'")
	conn, err := vm.Agent.Stream(e.ctx, agent.Exec{Argv: []string{"git", "bundle", "create", "-", "persona/test"}, Dir: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "x.bundle")
	f, _ := os.Create(bundle)
	io.Copy(f, conn)
	f.Close()
	conn.Close()
	repo := t.TempDir()
	run := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("fetch", "-q", bundle, "persona/test:persona/test")
	if msg := run("log", "-1", "--format=%s", "persona/test"); msg != "from the vm" {
		t.Errorf("bundle commit = %q", msg)
	}
}

// TestPty runs a program on a guest terminal and resizes it.
func TestPty(t *testing.T) {
	e := newEnv(t)
	vm := e.start("test-pty", net.IPv4(10, 200, 0, 35))
	conn, session, err := vm.Agent.Pty(e.ctx, agent.Exec{Rows: 30, Cols: 100,
		Argv: []string{"/bin/sh", "-c", "tty; stty size; read x; stty size"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	readUntil := func(want string) {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for !strings.Contains(out.String(), want) {
			n, err := conn.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				t.Fatalf("waiting for %q: %v; got %q", want, err, out.String())
			}
		}
	}
	readUntil("30 100")
	if !strings.Contains(out.String(), "/dev/pts/") {
		t.Errorf("not on a pty: %q", out.String())
	}
	if err := vm.Agent.Resize(e.ctx, session, 40, 120); err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("\n"))
	readUntil("40 120")
}

// TestGuestRequests sends guest-initiated requests to a host subscriber.
func TestGuestRequests(t *testing.T) {
	e := newEnv(t)
	vm := e.start("test-requests", net.IPv4(10, 200, 0, 40))

	if out, err := e.sh(vm, "persona-agent request push '{}'"); err == nil || !strings.Contains(out, "no host broker") {
		t.Errorf("without a subscriber: %q %v", out, err)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go vm.Agent.Subscribe(ctx, func(_ context.Context, ev agent.Event) (json.RawMessage, error) {
		if ev.Kind != "push" {
			return nil, fmt.Errorf("unsupported request %q", ev.Kind)
		}
		var body struct{ Branch string }
		json.Unmarshal(ev.Body, &body)
		return json.Marshal(map[string]string{"pushed": body.Branch})
	})
	var out string
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if out, err = e.sh(vm, `persona-agent request push '{"branch":"persona/x"}'`); err == nil {
			break
		}
	}
	if err != nil || out != `{"pushed":"persona/x"}` {
		t.Errorf("push: %q %v", out, err)
	}
	if out, err := e.sh(vm, "persona-agent request delete-repo"); err == nil || !strings.Contains(out, "unsupported") {
		t.Errorf("refused request: %q %v", out, err)
	}
}

func TestMain(m *testing.M) {
	// Acts as rsync's remote shell: `rsh vm rsync --server ...`.
	if sock := os.Getenv("PERSONA_TEST_STREAM"); sock != "" {
		args := os.Args[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-test.") {
			args = args[1:]
		}
		conn, err := agent.Client{Vsock: sock}.Stream(context.Background(), agent.Exec{Argv: args[1:]})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go io.Copy(conn, os.Stdin)
		io.Copy(os.Stdout, conn)
		os.Exit(0)
	}
	code := m.Run()
	if os.Getenv("PERSONA_FC_TEST") != "" {
		// Other packages' tests may run VMs concurrently; only ours count.
		out, _ := exec.Command("sudo", "-n", Wrapper, "list").Output()
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.Contains(line, `"id":"test-`) {
				fmt.Fprintf(os.Stderr, "leaked VM: %s\n", line)
				code = 1
			}
		}
	}
	os.Exit(code)
}
