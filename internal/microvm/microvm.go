//go:build linux

// Package microvm runs persona instances as Firecracker microVMs.
//
// Each VM runs under Firecracker's jailer, started by the root-owned
// persona-vm wrapper (cmd/persona-vm) through a narrow sudoers rule; this
// package never needs root itself. persona-vm hands back the VM's API socket,
// and this package configures, snapshots and restores the VM through it.
//
// All microVMs share one network namespace (see scripts/persona-netns.sh):
// each VM's tap device is attached to the bridge there, so VMs reach each
// other directly, and the host NATs their traffic to the internet while
// blocking the host itself and every private range on the local network.
package microvm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/johndavisdev/persona/internal/agent"
	"github.com/sirupsen/logrus"
)

// Wrapper is the only command persona runs as root; sudoers allows exactly it.
const Wrapper = "/usr/libexec/persona/persona-vm"

// Network layout created by scripts/persona-netns.sh. Keep the two in sync.
var (
	// Subnet is the bridge network every VM gets an address in.
	Subnet = net.IPNet{IP: net.IPv4(10, 200, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}
	// Gateway is the bridge address inside the namespace.
	Gateway = net.IPv4(10, 200, 0, 1).To4()
	// Nameservers: persona-dns on the bridge (which resolves spawn names in
	// the asking VM's namespace and forwards the rest), then a public
	// resolver in case it is down. VMs can't reach any LAN host,
	// including the LAN's DNS server.
	Nameservers = []string{"10.200.0.1", "1.1.1.1"}
	// SearchDomain makes a bare spawn name ("jill") resolve as jill.persona.
	SearchDomain = "persona"
)

// Paths inside the jail. They are identical for every VM, which is what lets
// one snapshot be restored into many jails.
const (
	jailKernel  = "vmlinux"
	jailRootfs  = "rootfs.ext4"
	jailOverlay = "overlay.ext4"
	jailMem     = "mem"
	jailState   = "vmstate"
	jailVsock   = "/run/vsock.sock"
	guestCID    = 3
)

// Spec describes one microVM to cold boot.
type Spec struct {
	ID     string // [a-z0-9-], unique among running VMs
	Kernel string // uncompressed vmlinux, world-readable
	// Rootfs is the read-only root image (see ExportRootfs), shared by every
	// VM through a hard link. It must contain persona-agent at AgentPath.
	Rootfs string
	// Overlay is this VM's writable layer (see NewOverlay), copied sparsely
	// into the jail. Without one, writes go to guest memory.
	Overlay string
	// Stores are the store layers (see Store) the guest mounts together on
	// /nix/store, hard linked into the jail read-only like Rootfs.
	Stores []string
	VCPUs  int64
	MemMiB int64
	IP     net.IP // address in Subnet
	// Namespace is the persona network namespace the VM joins ("" is
	// DefaultNamespace). Every namespace has the same Subnet and Gateway.
	Namespace string
	// Spawn and Persona name the VM for persona-dns: Spawn resolves to IP
	// for VMs in the same namespace.
	Spawn, Persona string
}

// VM is a running microVM.
type VM struct {
	ID      string
	IP      net.IP
	Tap     string
	API     string // Firecracker API socket
	Vsock   string // host side of the guest's vsock (CID 3)
	Console string // serial console log
	Agent   agent.Client
	spec    Spec
	client  *firecracker.Client
}

// wrapperInfo is what persona-vm start prints.
type wrapperInfo struct {
	ID      string `json:"id"`
	Tap     string `json:"tap"`
	API     string `json:"api_sock"`
	Vsock   string `json:"vsock_sock"`
	Console string `json:"console"`
}

// MAC derives a stable, locally administered MAC address from an IPv4 address.
func MAC(ip net.IP) string {
	v4 := ip.To4()
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

// KernelArgs runs persona-agent as init and configures eth0 statically via
// the kernel's ip= option (clones restored from a snapshot are re-addressed
// by the agent instead).
func KernelArgs(ip net.IP, hostname string) string {
	ns := append(append([]string{}, Nameservers...), "", "")
	return fmt.Sprintf("console=ttyS0 reboot=k panic=1 init=%s ip=%s::%s:%s:%s:eth0:off:%s:%s",
		AgentPath, ip.To4(), Gateway, net.IP(Subnet.Mask), hostname, ns[0], ns[1])
}

// Identity is what the agent applies to make this VM distinct: its address
// and MAC, hostname, the current time, and fresh entropy.
func Identity(id string, ip net.IP) agent.Identity {
	seed := make([]byte, 64)
	_, _ = rand.Read(seed)
	ones, _ := Subnet.Mask.Size()
	return agent.Identity{Addr: fmt.Sprintf("%s/%d", ip.To4(), ones), Gateway: Gateway.String(), MAC: MAC(ip),
		Hostname: id, Nameservers: Nameservers, Search: []string{SearchDomain}, TimeUnixNano: time.Now().UnixNano(), Seed: seed}
}

func (s Spec) validate() error {
	if s.ID == "" || s.Kernel == "" || s.Rootfs == "" {
		return fmt.Errorf("microvm: ID, Kernel and Rootfs are required")
	}
	return s.validateNet()
}

func (s Spec) validateNet() error {
	if s.IP.To4() == nil || !Subnet.Contains(s.IP) || s.IP.Equal(Gateway) {
		return fmt.Errorf("microvm: IP %v must be a host address in %v", s.IP, &Subnet)
	}
	return nil
}

// Start cold boots a microVM.
func Start(ctx context.Context, s Spec) (*VM, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if s.VCPUs == 0 {
		s.VCPUs = 2
	}
	if s.MemMiB == 0 {
		s.MemMiB = 2048
	}
	files := []string{"--file", "ro:" + s.Kernel + ":" + jailKernel, "--file", "ro:" + s.Rootfs + ":" + jailRootfs}
	if s.Overlay != "" {
		files = append(files, "--file", "rw:"+s.Overlay+":"+jailOverlay)
	}
	files = append(files, storeFiles(s.Stores)...)
	vm, err := launch(ctx, s, files...)
	if err != nil {
		return nil, err
	}
	if err := vm.configure(ctx); err != nil {
		vm.Stop()
		return nil, err
	}
	if err := vm.ready(ctx, 60*time.Second); err != nil {
		vm.Stop()
		return nil, err
	}
	return vm, nil
}

// ready waits for the agent and hands it the VM's identity.
func (v *VM) ready(ctx context.Context, limit time.Duration) error {
	if err := v.Agent.Wait(ctx, limit); err != nil {
		return fmt.Errorf("microvm %s: %w", v.ID, err)
	}
	if err := v.Agent.Identify(ctx, Identity(v.ID, v.IP)); err != nil {
		return fmt.Errorf("microvm %s: %w", v.ID, err)
	}
	return nil
}

func (v *VM) configure(ctx context.Context) error {
	s := v.spec
	steps := []func() error{
		func() error {
			_, err := v.client.PutMachineConfiguration(ctx, &models.MachineConfiguration{
				VcpuCount: firecracker.Int64(s.VCPUs), MemSizeMib: firecracker.Int64(s.MemMiB)})
			return err
		},
		func() error {
			_, err := v.client.PutGuestBootSource(ctx, &models.BootSource{
				KernelImagePath: firecracker.String(jailKernel), BootArgs: KernelArgs(s.IP, s.ID)})
			return err
		},
		func() error {
			_, err := v.client.PutGuestDriveByID(ctx, "rootfs", &models.Drive{DriveID: firecracker.String("rootfs"),
				PathOnHost: firecracker.String(jailRootfs), IsRootDevice: firecracker.Bool(true),
				IsReadOnly: firecracker.Bool(true)})
			return err
		},
		func() error {
			if s.Overlay == "" {
				return nil
			}
			_, err := v.client.PutGuestDriveByID(ctx, "overlay", &models.Drive{DriveID: firecracker.String("overlay"),
				PathOnHost: firecracker.String(jailOverlay), IsRootDevice: firecracker.Bool(false),
				IsReadOnly: firecracker.Bool(false)})
			return err
		},
		func() error {
			// After the overlay, so it stays /dev/vdb; the guest finds these by label.
			for i := range s.Stores {
				id := fmt.Sprintf("store%d", i)
				if _, err := v.client.PutGuestDriveByID(ctx, id, &models.Drive{DriveID: firecracker.String(id),
					PathOnHost: firecracker.String(jailStore(i)), IsRootDevice: firecracker.Bool(false),
					IsReadOnly: firecracker.Bool(true)}); err != nil {
					return err
				}
			}
			return nil
		},
		func() error {
			_, err := v.client.PutGuestNetworkInterfaceByID(ctx, "eth0", &models.NetworkInterface{
				IfaceID: firecracker.String("eth0"), HostDevName: firecracker.String(v.Tap), GuestMac: MAC(s.IP)})
			return err
		},
		func() error {
			_, err := v.client.PutGuestVsock(ctx, &models.Vsock{GuestCid: firecracker.Int64(guestCID),
				UdsPath: firecracker.String(jailVsock)})
			return err
		},
		func() error {
			_, err := v.client.CreateSyncAction(ctx, &models.InstanceActionInfo{
				ActionType: firecracker.String(models.InstanceActionInfoActionTypeInstanceStart)})
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("microvm %s: configure: %w", v.ID, err)
		}
	}
	return nil
}

// Snapshot is a paused VM's saved state, collected out of its jail.
type Snapshot struct {
	Dir     string // holds mem, vmstate, and overlay.ext4 if the VM had an overlay
	Rootfs  string // the shared read-only image the snapshot was taken on
	Stores  []string
	Overlay bool
	VCPUs   int64
	MemMiB  int64
}

// Snapshot quiesces the guest (disks flushed, eth0 down), pauses it, and
// saves a full snapshot into dir, an existing directory owned by the caller
// on the same filesystem as /srv/jailer. The VM stays paused. The overlay disk
// is saved too, since the guest's page cache in memory refers to it.
func (v *VM) Snapshot(ctx context.Context, dir string) (Snapshot, error) {
	if err := v.Agent.Quiesce(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("microvm %s: %w", v.ID, err)
	}
	if _, err := v.client.PatchVM(ctx, &models.VM{State: firecracker.String(models.VMStatePaused)}); err != nil {
		return Snapshot{}, fmt.Errorf("microvm %s: pause: %w", v.ID, err)
	}
	if _, err := v.client.CreateSnapshot(ctx, &models.SnapshotCreateParams{
		MemFilePath: firecracker.String(jailMem), SnapshotPath: firecracker.String(jailState)}); err != nil {
		return Snapshot{}, fmt.Errorf("microvm %s: snapshot: %w", v.ID, err)
	}
	names := []string{jailMem, jailState}
	if v.spec.Overlay != "" {
		names = append(names, jailOverlay)
	}
	if err := wrapper(ctx, nil, append([]string{"collect", v.ID, "--to", dir}, names...)...); err != nil {
		return Snapshot{}, err
	}
	for _, n := range names {
		if err := os.Chmod(filepath.Join(dir, n), 0o644); err != nil {
			return Snapshot{}, err
		}
	}
	return Snapshot{Dir: dir, Rootfs: v.spec.Rootfs, Stores: v.spec.Stores, Overlay: v.spec.Overlay != "",
		VCPUs: v.spec.VCPUs, MemMiB: v.spec.MemMiB}, nil
}

// Restore starts a new VM from a snapshot as id at ip, resumes it, and gives
// it its own identity. Guest memory is hard linked into the jail and mapped
// privately, so clones of one snapshot share its pages until they write them.
func Restore(ctx context.Context, snap Snapshot, id string, ip net.IP) (*VM, error) {
	s := Spec{ID: id, Rootfs: snap.Rootfs, Stores: snap.Stores, IP: ip, VCPUs: snap.VCPUs, MemMiB: snap.MemMiB}
	if err := s.validateNet(); err != nil {
		return nil, err
	}
	files := []string{
		"--file", "ro:" + filepath.Join(snap.Dir, jailMem) + ":" + jailMem,
		"--file", "ro:" + filepath.Join(snap.Dir, jailState) + ":" + jailState,
		"--file", "ro:" + snap.Rootfs + ":" + jailRootfs,
	}
	if snap.Overlay {
		s.Overlay = filepath.Join(snap.Dir, jailOverlay)
		files = append(files, "--file", "rw:"+s.Overlay+":"+jailOverlay)
	}
	files = append(files, storeFiles(snap.Stores)...)
	vm, err := launch(ctx, s, files...)
	if err != nil {
		return nil, err
	}
	// The SDK's models predate network_overrides, so this call is made directly.
	err = vm.put(ctx, "/snapshot/load", map[string]any{
		"snapshot_path":     jailState,
		"mem_backend":       map[string]string{"backend_type": "File", "backend_path": jailMem},
		"resume_vm":         true,
		"clock_realtime":    true,
		"network_overrides": []map[string]string{{"iface_id": "eth0", "host_dev_name": vm.Tap}},
	})
	if err == nil {
		err = vm.ready(ctx, 10*time.Second)
	}
	if err != nil {
		vm.Stop()
		return nil, fmt.Errorf("microvm %s: restore: %w", id, err)
	}
	return vm, nil
}

// jailStore is store layer i's name in the jail (the same in every jail, so
// snapshots restore anywhere).
func jailStore(i int) string { return fmt.Sprintf("store-%d.ext4", i) }

func storeFiles(stores []string) []string {
	var files []string
	for i, st := range stores {
		files = append(files, "--file", "ro:"+st+":"+jailStore(i))
	}
	return files
}

// launch asks persona-vm for a jailed Firecracker and connects to its API.
func launch(ctx context.Context, s Spec, files ...string) (*VM, error) {
	var out bytes.Buffer
	args := []string{"start", "--id", s.ID}
	if s.Namespace != "" {
		args = append(args, "--ns", s.Namespace)
	}
	if s.IP != nil {
		args = append(args, "--ip", s.IP.String())
	}
	if s.Spawn != "" {
		args = append(args, "--name", s.Spawn, "--persona", s.Persona)
	}
	if err := wrapper(ctx, &out, append(args, files...)...); err != nil {
		return nil, err
	}
	var in wrapperInfo
	if err := json.Unmarshal(out.Bytes(), &in); err != nil {
		return nil, fmt.Errorf("persona-vm start: %w: %s", err, out.String())
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	vm := &VM{ID: in.ID, IP: s.IP, Tap: in.Tap, API: in.API, Vsock: in.Vsock, Console: in.Console, spec: s,
		Agent: agent.Client{Vsock: in.Vsock}, client: firecracker.NewClient(in.API, logrus.NewEntry(logger), false)}
	// The socket exists once Firecracker has bound it; wait until it accepts.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := vm.client.GetFirecrackerVersion(ctx); err == nil {
			return vm, nil
		} else if time.Now().After(deadline) {
			vm.Stop()
			return nil, fmt.Errorf("microvm %s: API never came up: %w", s.ID, err)
		}
	}
}

// Stop kills the VM and removes its jail, tap, and sockets.
func (v *VM) Stop() error { return wrapper(context.Background(), nil, "stop", v.ID) }

// ConsoleLog returns what the guest has written to its serial console.
func (v *VM) ConsoleLog() string {
	b, _ := os.ReadFile(v.Console)
	return string(b)
}

func (v *VM) put(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	hc := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", v.API)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func wrapper(ctx context.Context, stdout io.Writer, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", Wrapper}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("persona-vm %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// userDir is where persona-vm exposes a running VM's sockets to its owner.
func userDir(id string) string {
	return filepath.Join("/srv/jailer/persona-users", strconv.Itoa(os.Getuid()), id)
}

// Lookup returns a handle to a running VM started by this user, without
// asking persona-vm (no sudo), or false if it isn't running.
func Lookup(id string) (*VM, bool) {
	d := userDir(id)
	if fi, err := os.Lstat(filepath.Join(d, "api.sock")); err != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil, false
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	api := filepath.Join(d, "api.sock")
	vs := filepath.Join(d, "vsock.sock")
	return &VM{ID: id, API: api, Vsock: vs, Console: filepath.Join(d, "console.log"), Agent: agent.Client{Vsock: vs},
		client: firecracker.NewClient(api, logrus.NewEntry(logger), false)}, true
}

// Running lists the ids of this user's running VMs.
func Running(ctx context.Context) (map[string]bool, error) {
	var out bytes.Buffer
	if err := wrapper(ctx, &out, "list"); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var in wrapperInfo
		if err := dec.Decode(&in); err != nil {
			return nil, err
		}
		ids[in.ID] = true
	}
	return ids, nil
}

// Stop stops a VM by id.
func Stop(ctx context.Context, id string) error { return wrapper(ctx, nil, "stop", id) }

// Available reports whether this host can run persona microVMs.
func Available(ctx context.Context, kernel string) error {
	if _, err := os.Stat(Wrapper); err != nil {
		return fmt.Errorf("microVMs need the persona package (%s is missing): install persona_*.deb, or set runtime: docker", Wrapper)
	}
	if _, err := os.Stat("/run/netns/persona"); err != nil {
		return fmt.Errorf("the persona network namespace is down: sudo systemctl start persona-netns")
	}
	if fi, err := os.Stat(kernel); err != nil || fi.Mode().Perm()&0o004 == 0 {
		return fmt.Errorf("guest kernel %s is missing or not world-readable (set vm_kernel, or reinstall the persona package)", kernel)
	}
	if err := wrapper(ctx, io.Discard, "list"); err != nil {
		return fmt.Errorf("cannot run %s via sudo: %w", Wrapper, err)
	}
	return nil
}

// DiskName is the writable disk's file name, inside the jail and when saved.
const DiskName = jailOverlay

// SaveDisk pauses the VM and copies its writable disk into dir (replacing any
// earlier copy). Flush the guest's filesystems first (agent quiesce).
func (v *VM) SaveDisk(ctx context.Context, dir string) error {
	if _, err := v.client.PatchVM(ctx, &models.VM{State: firecracker.String(models.VMStatePaused)}); err != nil {
		return fmt.Errorf("microvm %s: pause: %w", v.ID, err)
	}
	_ = os.Remove(filepath.Join(dir, jailOverlay))
	return wrapper(ctx, nil, "collect", v.ID, "--to", dir, jailOverlay)
}
