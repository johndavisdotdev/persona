//go:build !linux

package instance

import (
	"context"
	"errors"
	"net"
)

// BridgeSocket needs the microVM bridge, which only exists on Linux.
func BridgeSocket(ctx context.Context, namespace string) (*net.UDPConn, error) {
	return nil, errors.New("the microVM bridge needs Linux")
}
