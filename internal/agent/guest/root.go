//go:build linux

package guest

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	stage      = "/run/stage"
	overlayDev = "/dev/vdb"
	// ext4 labels of the disks besides the root (see package microvm).
	overlayLabel = "persona-overlay"
	storeLabel   = "persona-store"
)

// ext4Label reads an ext4 filesystem's volume label from its superblock.
func ext4Label(dev string) string {
	f, err := os.Open(dev)
	if err != nil {
		return ""
	}
	defer f.Close()
	sb := make([]byte, 136)
	if _, err := f.ReadAt(sb, 1024); err != nil || sb[0x38] != 0x53 || sb[0x39] != 0xef {
		return ""
	}
	return string(bytes.TrimRight(sb[0x78:0x88], "\x00"))
}

// disks finds the VM's writable disk and store layers among /dev/vd[b-z].
func disks() (overlay string, stores []string) {
	for c := 'b'; c <= 'z'; c++ {
		dev := "/dev/vd" + string(c)
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		switch ext4Label(dev) {
		case overlayLabel:
			overlay = dev
		case storeLabel:
			stores = append(stores, dev)
		}
	}
	if overlay == "" {
		if _, err := os.Stat(overlayDev); err == nil && !strings.Contains(strings.Join(stores, " "), overlayDev) {
			overlay = overlayDev // an unlabeled writable disk (older overlays)
		}
	}
	return overlay, stores
}

// setupRoot pivots into an overlay of the read-only root image (lower) and
// this VM's own disk (upper), and mounts the store layers on /nix/store.
// Without a disk the upper layer is tmpfs.
func setupRoot() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	if err := unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount /run: %w", err)
	}
	lower, ovl, root := stage+"/lower", stage+"/ovl", stage+"/root"
	for _, d := range []string{lower, ovl, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// A non-recursive bind of / is just the root image, without /run etc.
	if err := unix.Mount("/", lower, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind lower: %w", err)
	}
	overlay, stores := disks()
	if overlay != "" {
		if err := unix.Mount(overlay, ovl, "ext4", unix.MS_NOATIME, ""); err != nil {
			return fmt.Errorf("mount %s: %w", overlay, err)
		}
	} else {
		log.Printf("no writable disk: writes go to memory")
	}
	var layers []string
	for i, dev := range stores {
		dir := fmt.Sprintf("%s/store-%d", stage, i)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(dev, dir, "ext4", unix.MS_RDONLY|unix.MS_NOATIME, ""); err != nil {
			return fmt.Errorf("mount store layer %s: %w", dev, err)
		}
		layers = append(layers, "/.oldroot"+dir) // where it is after the pivot
	}
	for _, d := range []string{ovl + "/upper", ovl + "/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s/upper,workdir=%s/work", lower, ovl, ovl)
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	old := root + "/.oldroot"
	if err := os.MkdirAll(old, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(root, old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := mountStore(layers); err != nil {
		return err
	}
	// The overlays keep their layers alive; the old tree can go.
	return unix.Unmount("/.oldroot", unix.MNT_DETACH)
}

// mountStore puts the store layers together on /nix/store, read-only.
func mountStore(layers []string) error {
	if len(layers) == 0 {
		return nil
	}
	if err := os.MkdirAll("/nix/store", 0o755); err != nil {
		return err
	}
	if len(layers) == 1 {
		if err := unix.Mount(layers[0], "/nix/store", "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind /nix/store: %w", err)
		}
		return unix.Mount("", "/nix/store", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	}
	// An overlay of lower layers only is read-only.
	if err := unix.Mount("overlay", "/nix/store", "overlay", unix.MS_RDONLY, "lowerdir="+strings.Join(layers, ":")); err != nil {
		return fmt.Errorf("mount /nix/store: %w", err)
	}
	return nil
}

// mountSystem mounts the pseudo filesystems inside the (new) root.
func mountSystem() {
	type m struct {
		src, dst, fstype string
		flags            uintptr
		data             string
		mode             os.FileMode
	}
	const nosuid, nodev, noexec = unix.MS_NOSUID, unix.MS_NODEV, unix.MS_NOEXEC
	for _, x := range []m{
		{"proc", "/proc", "proc", nosuid | nodev | noexec, "", 0o555},
		{"sysfs", "/sys", "sysfs", nosuid | nodev | noexec, "", 0o555},
		{"devtmpfs", "/dev", "devtmpfs", nosuid, "mode=0755", 0o755},
		{"devpts", "/dev/pts", "devpts", nosuid | noexec, "newinstance,ptmxmode=0666,mode=0620,gid=5", 0o755},
		{"tmpfs", "/dev/shm", "tmpfs", nosuid | nodev, "mode=1777", 0o1777},
		{"tmpfs", "/run", "tmpfs", nosuid | nodev, "mode=0755", 0o755},
		{"tmpfs", "/tmp", "tmpfs", nosuid | nodev, "mode=1777", 0o1777},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", nosuid | nodev | noexec, "", 0o555},
	} {
		_ = os.MkdirAll(x.dst, x.mode)
		if err := unix.Mount(x.src, x.dst, x.fstype, x.flags, x.data); err != nil && err != unix.EBUSY {
			log.Printf("mount %s: %v", x.dst, err)
		}
	}
	_ = os.Remove("/dev/ptmx")
	_ = os.Symlink("pts/ptmx", "/dev/ptmx")
	for name, target := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		_ = os.Symlink(target, "/dev/"+name)
	}
	_ = os.MkdirAll("/run/persona", 0o755)
	_ = os.Symlink("/proc/self/mounts", "/etc/mtab")
}
