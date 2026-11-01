//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// bridgeAddr is fcbr0's address in the persona namespace (persona-netns).
var bridgeAddr = [4]byte{10, 200, 0, 1}

// cmdMcastSocket hands the caller a UDP socket inside a persona network
// namespace (--ns, default "default"), bound to the bridge address with multicast sent out the bridge.
// The caller's stdin must be a Unix socket; the descriptor goes back over it
// (SCM_RIGHTS) and this process exits. A socket stays in the namespace it was
// created in, so the unprivileged caller can then send multicast to the VMs
// and receive their unicast replies itself.
//
// This gives the caller no more than a VM has: a host on the shared bridge.
func cmdMcastSocket(args []string) error {
	fs := flag.NewFlagSet("mcast-socket", flag.ContinueOnError)
	nsName := fs.String("ns", defaultNamespace, "persona network namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		usage()
	}
	var st unix.Stat_t
	if err := unix.Fstat(0, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return errors.New("mcast-socket: stdin must be a Unix socket to receive the descriptor")
	}
	if err := namespaceUp(*nsName); err != nil {
		return err
	}
	// Namespaces are per thread: this one stays in the netns until it exits.
	runtime.LockOSThread()
	ns, err := unix.Open(netnsPath(*nsName), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := unix.Setns(ns, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("enter netns %s: %w", netnsOf(*nsName), err)
	}
	unix.Close(ns)

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	for _, o := range []struct {
		opt, val int
	}{{unix.IP_MULTICAST_TTL, 1}, {unix.IP_MULTICAST_LOOP, 0}} {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, o.opt, o.val); err != nil {
			return err
		}
	}
	if err := unix.SetsockoptInet4Addr(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, bridgeAddr); err != nil {
		return fmt.Errorf("multicast interface %s: %w", bridge, err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: bridgeAddr}); err != nil {
		return fmt.Errorf("bind on %s: %w", bridge, err)
	}
	if err := unix.Sendmsg(0, []byte{0}, unix.UnixRights(fd), nil, 0); err != nil {
		return fmt.Errorf("hand back the socket: %w", err)
	}
	return nil
}
