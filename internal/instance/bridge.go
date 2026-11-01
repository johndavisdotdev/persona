//go:build linux

package instance

import (
	"context"
	"net"

	"github.com/johndavisdev/persona/internal/microvm"
)

// BridgeSocket is a UDP socket on the bridge the microVMs of a namespace
// share (see microvm.BridgeSocket), for speaking persona-beacon's multicast
// protocol.
func BridgeSocket(ctx context.Context, namespace string) (*net.UDPConn, error) {
	return microvm.BridgeSocket(ctx, namespace)
}
