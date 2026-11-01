package beaconnet

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/johndavisdev/persona/shapes"
)

func TestAddressRoundTrip(t *testing.T) {
	a := Address(net.IPv4(10, 200, 0, 5))
	if got := a.GetIpv4(); got != 0x0AC80005 {
		t.Errorf("ipv4 = %#x, want 0x0ac80005 (big-endian, as the beacon reads it)", got)
	}
	if ip := IP(a); !ip.Equal(net.IPv4(10, 200, 0, 5)) {
		t.Errorf("round trip = %v", ip)
	}
}

// A fake beacon on loopback stands in for the multicast group.
func TestDiscover(t *testing.T) {
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	old := Group
	Group = peer.LocalAddr().(*net.UDPAddr)
	defer func() { Group = old }()

	go func() {
		buf := make([]byte, 1500)
		n, from, err := peer.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req := &shapes.MulticastRequest{}
		if proto.Unmarshal(buf[:n], req) != nil || req.GetDiscovery() == nil {
			return
		}
		for _, name := range []string{"alpha", "alpha", "beta"} { // a duplicate answer
			b, _ := proto.Marshal(&shapes.AgentAnnouncement{Name: name, Persona: "p"})
			peer.WriteToUDP(b, from)
		}
		peer.WriteToUDP([]byte{0xff, 0xff}, from) // garbage is skipped
	}()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	agents, err := Discover(context.Background(), conn, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].GetName() != "alpha" || agents[1].GetName() != "beta" {
		t.Fatalf("agents = %v", agents)
	}
	if ip := IP(agents[0].GetAddress()); !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("address from source = %v", ip)
	}
}

func TestCommandEncoding(t *testing.T) {
	peer, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer peer.Close()
	old := Group
	Group = peer.LocalAddr().(*net.UDPAddr)
	defer func() { Group = old }()
	conn, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer conn.Close()
	if err := Command(conn, "stop all"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	peer.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	req := &shapes.MulticastRequest{}
	if err := proto.Unmarshal(buf[:n], req); err != nil || req.GetCommand().GetCommand() != "stop all" {
		t.Fatalf("decoded %v, %v", req, err)
	}
}
