package cmd

import "testing"

func TestParseFrom(t *testing.T) {
	for _, c := range []struct {
		in, ip, proto string
		port          int
		bad           bool
	}{
		{in: "localhost:8080", ip: "127.0.0.1", port: 8080, proto: "tcp"},
		{in: "172.16.1.100:80:tcp", ip: "172.16.1.100", port: 80, proto: "tcp"},
		{in: "localhost:53:UDP", ip: "127.0.0.1", port: 53, proto: "udp"},
		{in: "cgnat:80", ip: "cgnat", port: 80, proto: "tcp"},
		{in: "localhost", bad: true},
		{in: "localhost:0", bad: true},
		{in: "localhost:70000", bad: true},
		{in: "localhost:80:sctp", bad: true},
		{in: "example.com:80", bad: true},
		{in: "::1:80", bad: true},
	} {
		ip, port, proto, err := parseFrom(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseFrom(%q) accepted", c.in)
			}
			continue
		}
		if err != nil || ip != c.ip || port != c.port || proto != c.proto {
			t.Errorf("parseFrom(%q) = %s %d %s %v", c.in, ip, port, proto, err)
		}
	}
}

func TestParseTo(t *testing.T) {
	if s, p, err := parseTo("jill:8080"); err != nil || s != "jill" || p != 8080 {
		t.Errorf("parseTo = %s %d %v", s, p, err)
	}
	for _, bad := range []string{"jill", ":80", "jill:x", ""} {
		if _, _, err := parseTo(bad); err == nil {
			t.Errorf("parseTo(%q) accepted", bad)
		}
	}
}
