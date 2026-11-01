//go:build linux

package microvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// BridgeSocket returns a UDP socket inside a persona network namespace ("" is
// DefaultNamespace), bound to the bridge address (Gateway) on an ephemeral
// port, with multicast sent out the bridge to every VM in that namespace. The caller can't enter the namespace
// itself: persona-vm creates the socket there and passes it back over a
// socketpair on its stdin (sudo keeps stdin when it allocates a pty).
func BridgeSocket(ctx context.Context, namespace string) (*net.UDPConn, error) {
	if _, err := os.Stat(Wrapper); err != nil {
		return nil, fmt.Errorf("the microVM bridge needs the persona package (%s is missing)", Wrapper)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ours, theirs := os.NewFile(uintptr(pair[0]), "mcast-ours"), os.NewFile(uintptr(pair[1]), "mcast-theirs")
	defer ours.Close()

	args := []string{"-n", Wrapper, "mcast-socket"}
	if namespace != "" {
		args = append(args, "--ns", namespace)
	}
	cmd := exec.CommandContext(ctx, "sudo", args...)
	cmd.Stdin = theirs
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	theirs.Close()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s mcast-socket: %s", Wrapper, msg)
		}
		return nil, fmt.Errorf("%s mcast-socket: %w", Wrapper, err)
	}

	buf, oob := make([]byte, 1), make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := unix.Recvmsg(int(ours.Fd()), buf, oob, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("receive the bridge socket: %w", err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return nil, errors.New("persona-vm passed back no socket")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return nil, errors.New("persona-vm passed back no socket")
	}
	f := os.NewFile(uintptr(fds[0]), "persona-bridge")
	defer f.Close() // FilePacketConn dups it
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, errors.New("persona-vm passed back a non-UDP socket")
	}
	return conn, nil
}
