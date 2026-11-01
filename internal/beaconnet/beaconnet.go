// Package beaconnet speaks persona-beacon's multicast protocol
// (protos/shapes.proto) on the microVM bridge: discovery of running agents
// and commands broadcast to all of them. Datagrams are single protobuf
// messages, encoded as is.
package beaconnet

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/johndavisdev/persona/shapes"
)

// Group is where every beacon listens (the beacon's MulticastOptions defaults).
var Group = &net.UDPAddr{IP: net.IPv4(239, 255, 77, 1), Port: 47777}

// maxDatagram bounds what a peer can send back.
const maxDatagram = 64 << 10

// Send multicasts one request to every agent on the bridge.
func Send(conn *net.UDPConn, req *shapes.MulticastRequest) error {
	b, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDP(b, Group)
	return err
}

// Command broadcasts a command to every agent.
func Command(conn *net.UDPConn, command string) error {
	return Send(conn, commandRequest(command))
}

// CommandTo sends a command to one agent, by unicast to its address (beacons
// listen on the group's port on every address).
func CommandTo(conn *net.UDPConn, ip net.IP, command string) error {
	if ip == nil {
		return errors.New("no address to send to")
	}
	b, err := proto.Marshal(commandRequest(command))
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDP(b, &net.UDPAddr{IP: ip, Port: Group.Port})
	return err
}

func commandRequest(command string) *shapes.MulticastRequest {
	return &shapes.MulticastRequest{RequestType: &shapes.MulticastRequest_Command{
		Command: &shapes.CommandFromGod{Command: command}}}
}

// Discover asks every agent to announce itself and collects the answers that
// arrive within wait (or until ctx ends). Peers answer by unicast to conn's
// address, so conn must be one they can reach (microvm.BridgeSocket).
func Discover(ctx context.Context, conn *net.UDPConn, wait time.Duration) ([]*shapes.AgentAnnouncement, error) {
	req := &shapes.MulticastRequest{RequestType: &shapes.MulticastRequest_Discovery{
		Discovery: &shapes.DiscoverAgentsRequest{}}} // answer to the source address
	if err := Send(conn, req); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	defer conn.SetReadDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()

	var out []*shapes.AgentAnnouncement
	seen := map[string]bool{}
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return out, ctx.Err()
		} else if err != nil {
			return out, err
		}
		a := &shapes.AgentAnnouncement{}
		if proto.Unmarshal(buf[:n], a) != nil || a.GetName() == "" {
			continue // not an announcement
		}
		if a.GetAddress() == nil {
			a.Address = Address(from.IP)
		}
		if key := a.GetName() + "@" + IP(a.GetAddress()).String(); !seen[key] {
			seen[key] = true
			out = append(out, a)
		}
	}
}

// Address converts an IP to the IpAddress message: ipv4 is the address read
// as a big-endian uint32, as the beacon does.
func Address(ip net.IP) *shapes.IpAddress {
	if v4 := ip.To4(); v4 != nil {
		return &shapes.IpAddress{Address: &shapes.IpAddress_Ipv4{Ipv4: binary.BigEndian.Uint32(v4)}}
	}
	return &shapes.IpAddress{Address: &shapes.IpAddress_Ipv6{Ipv6: append([]byte(nil), ip.To16()...)}}
}

// IP converts an IpAddress message back, or returns nil.
func IP(a *shapes.IpAddress) net.IP {
	switch v := a.GetAddress().(type) {
	case *shapes.IpAddress_Ipv4:
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, v.Ipv4)
		return ip
	case *shapes.IpAddress_Ipv6:
		if len(v.Ipv6) == net.IPv6len {
			return net.IP(v.Ipv6)
		}
	}
	return nil
}
