//go:build linux

package main

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestSpawnQuery(t *testing.T) {
	for _, c := range []struct {
		q, label, ns string
		inDomain, ok bool
	}{
		{"jill.", "jill", "", false, true},
		{"JILL.persona.", "jill", "", true, true},
		{"example.com.", "", "", false, false},
		{"jill.lab.persona.", "jill", "lab", true, true},
		{"a.b.c.persona.", "", "", true, false},
		{"persona.", "persona", "", false, true},
	} {
		label, ns, in, ok := spawnQuery(c.q)
		if label != c.label || ns != c.ns || in != c.inDomain || ok != c.ok {
			t.Errorf("spawnQuery(%q) = %q %q %v %v", c.q, label, ns, in, ok)
		}
	}
}

// fakeWriter captures the reply.
type fakeWriter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

func (f *fakeWriter) WriteMsg(m *dns.Msg) error { f.msg = m; return nil }
func (f *fakeWriter) LocalAddr() net.Addr       { return &net.UDPAddr{} }

func TestLookupIsPerNamespace(t *testing.T) {
	d := &dnsServer{cacheAt: time.Now(), cache: []state{
		{ID: "persona-dev-jill", Spawn: "jill", IP: "10.200.1.1"},                   // default
		{ID: "persona-dev-jack", Spawn: "jack", IP: "10.200.1.2", Namespace: "lab"}, // lab
	}}
	ask := func(ns, name string) *dns.Msg {
		r := new(dns.Msg)
		r.SetQuestion(name, dns.TypeA)
		w := &fakeWriter{}
		d.handler(ns)(w, r)
		return w.msg
	}
	if m := ask("default", "jill."); m == nil || len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "10.200.1.1" {
		t.Errorf("jill from default: %v", m)
	}
	if m := ask("default", "persona-dev-jill.persona."); m == nil || len(m.Answer) != 1 {
		t.Errorf("VM id from default: %v", m)
	}
	// jack is in lab: invisible from default, and .persona names don't leak upstream.
	if m := ask("default", "jack.persona."); m == nil || m.Rcode != dns.RcodeNameError || len(m.Answer) != 0 {
		t.Errorf("jack from default: %v", m)
	}
	if m := ask("lab", "jack."); m == nil || len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "10.200.1.2" {
		t.Errorf("jack from lab: %v", m)
	}
	// Qualified by namespace: the CGNAT address, from any namespace.
	if m := ask("default", "jack.lab.persona."); m == nil || len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "100.64.1.2" {
		t.Errorf("jack.lab.persona from default: %v", m)
	}
	if m := ask("default", "jack.default.persona."); m == nil || m.Rcode != dns.RcodeNameError {
		t.Errorf("jack.default.persona: %v", m)
	}
}
