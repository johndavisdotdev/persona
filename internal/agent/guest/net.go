//go:build linux

package guest

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"unsafe"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const iface = "eth0"

// coldBootNetwork keeps the kernel's ip= configuration and publishes its DNS.
func coldBootNetwork() {
	if b, err := os.ReadFile("/proc/net/pnp"); err == nil {
		writeResolv(string(b))
	}
	if l, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(l)
	}
}

func writeResolv(content string) {
	_ = os.Remove("/etc/resolv.conf") // may be a dangling symlink in the image
	if err := os.WriteFile("/etc/resolv.conf", []byte(content), 0o644); err != nil {
		log.Printf("resolv.conf: %v", err)
	}
}

// identify applies a clone's identity. Order matters: the clock and RNG are
// fixed before anything else runs, and eth0 only comes up with the new MAC.
func identify(id agent.Identity) error {
	if id.TimeUnixNano > 0 {
		ts := unix.NsecToTimespec(id.TimeUnixNano)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			return fmt.Errorf("set clock: %w", err)
		}
	}
	if len(id.Seed) > 0 {
		if err := addEntropy(id.Seed); err != nil {
			return fmt.Errorf("seed rng: %w", err)
		}
	}
	if id.Hostname != "" {
		if err := unix.Sethostname([]byte(id.Hostname)); err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
		_ = os.WriteFile("/etc/hostname", []byte(id.Hostname+"\n"), 0o644)
		_ = os.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost\n127.0.1.1 "+id.Hostname+"\n"), 0o644)
	}
	if id.Addr == "" {
		return nil
	}
	addr, err := netlink.ParseAddr(id.Addr)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetDown(link); err != nil {
		return err
	}
	if id.MAC != "" {
		mac, err := net.ParseMAC(id.MAC)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
			return fmt.Errorf("set mac: %w", err)
		}
	}
	old, _ := netlink.AddrList(link, netlink.FAMILY_V4)
	for _, a := range old {
		_ = netlink.AddrDel(link, &a)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("add addr: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	if gw := net.ParseIP(id.Gateway); gw != nil {
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
			return fmt.Errorf("default route: %w", err)
		}
	}
	if len(id.Nameservers) > 0 {
		writeResolv("nameserver " + strings.Join(id.Nameservers, "\nnameserver ") + "\n")
	}
	return nil
}

// quiesce flushes disks and takes eth0 down ahead of a snapshot.
func quiesce() error {
	unix.Sync()
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}
	return netlink.LinkSetDown(link)
}

// addEntropy credits seed to the kernel RNG (RNDADDENTROPY).
func addEntropy(seed []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 8+len(seed))
	*(*int32)(unsafe.Pointer(&buf[0])) = int32(len(seed) * 8) // entropy_count, bits
	*(*int32)(unsafe.Pointer(&buf[4])) = int32(len(seed))     // buf_size, bytes
	copy(buf[8:], seed)
	const rndAddEntropy = 0x40085203
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return e
	}
	return nil
}
