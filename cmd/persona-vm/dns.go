//go:build linux

package main

import (
	"log"
	"net"
	"os"
	osexec "os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

// persona-dns (persona-vm dns, run by persona-dns.service) is one DNS server
// for every persona namespace. It listens on the bridge address (10.200.0.1,
// UDP and TCP port 53) inside each namespace, so it knows which namespace a
// query came from, and answers a spawn's name ("jill", "jill.persona", or
// its VM id) with that spawn's address only if it runs in the same
// namespace. "<spawn>.<namespace>.persona" answers from any namespace with
// the spawn's CGNAT address (100.64.a.b for 10.200.a.b), where persona fw
// allow-from cgnat:... exposes its ports to other spawns. Every other query
// is forwarded to public resolvers.
//
// Spawns are read from persona-vm's own VM state (start --name / --ip):
// running VMs only. Namespaces are picked up (and dropped) as persona-netns
// creates and removes them.

const (
	dnsDomain = "persona"
	dnsTTL    = 5
	dnsListen = "10.200.0.1:53" // the bridge address in every namespace
)

var (
	dnsUpstreams = []string{"1.1.1.1:53", "8.8.8.8:53"}
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

type dnsServer struct {
	mu      sync.Mutex
	cache   []state
	cacheAt time.Time
}

// nsListener is one namespace's servers, and the netns they were opened in
// (a namespace removed and created again under the same name is new).
type nsListener struct {
	ino     uint64
	servers []*dns.Server
}

func cmdDNS(args []string) error {
	if len(args) != 0 {
		usage()
	}
	log.SetFlags(0)
	d := &dnsServer{}
	served := map[string]*nsListener{}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		d.syncNamespaces(served)
		select {
		case <-stop:
			for _, l := range served {
				l.shutdown()
			}
			return nil
		case <-tick.C:
		}
	}
}

// syncNamespaces opens servers in new namespaces and closes those of
// namespaces that are gone.
func (d *dnsServer) syncNamespaces(served map[string]*nsListener) {
	out, err := netnsOutput("list")
	if err != nil {
		log.Printf("listing namespaces: %v", err)
		return
	}
	present := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		name, path := f[0], "/run/netns/"+f[1]
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			continue // not up (yet)
		}
		present[name] = true
		if l := served[name]; l != nil {
			if l.ino == st.Ino {
				continue
			}
			l.shutdown() // recreated
			delete(served, name)
		}
		l, err := d.listen(name, path, st.Ino)
		if err != nil {
			log.Printf("namespace %s: %v (retrying)", name, err)
			continue
		}
		served[name] = l
		log.Printf("serving namespace %s", name)
	}
	for name, l := range served {
		if !present[name] {
			l.shutdown()
			delete(served, name)
			log.Printf("namespace %s is gone", name)
		}
	}
}

func (l *nsListener) shutdown() {
	for _, s := range l.servers {
		_ = s.Shutdown()
	}
}

// listen opens UDP and TCP sockets inside a network namespace and serves them.
func (d *dnsServer) listen(name, netnsPath string, ino uint64) (*nsListener, error) {
	type sockets struct {
		pc  net.PacketConn
		l   net.Listener
		err error
	}
	ch := make(chan sockets, 1)
	go func() {
		// Namespaces are per thread. This one is never unlocked, so the
		// thread exits with the goroutine instead of going back to the pool
		// inside the namespace.
		runtime.LockOSThread()
		fd, err := unix.Open(netnsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			ch <- sockets{err: err}
			return
		}
		defer unix.Close(fd)
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			ch <- sockets{err: err}
			return
		}
		pc, err := net.ListenPacket("udp4", dnsListen)
		if err != nil {
			ch <- sockets{err: err}
			return
		}
		l, err := net.Listen("tcp4", dnsListen)
		if err != nil {
			pc.Close()
			ch <- sockets{err: err}
			return
		}
		ch <- sockets{pc: pc, l: l}
	}()
	s := <-ch
	if s.err != nil {
		return nil, s.err
	}
	h := d.handler(name)
	udp := &dns.Server{PacketConn: s.pc, Handler: h}
	tcp := &dns.Server{Listener: s.l, Handler: h}
	for _, srv := range []*dns.Server{udp, tcp} {
		go func(srv *dns.Server) {
			if err := srv.ActivateAndServe(); err != nil {
				log.Printf("namespace %s: %v", name, err)
			}
		}(srv)
	}
	return &nsListener{ino: ino, servers: []*dns.Server{udp, tcp}}, nil
}

// spawnQuery reports whether qname asks for a spawn: a single label, a
// single label under the persona domain, or <spawn>.<namespace>.persona
// (namespace set). inDomain is true under the persona domain, which is
// answered here even when nothing matches.
func spawnQuery(qname string) (label, namespace string, inDomain, ok bool) {
	n := strings.TrimSuffix(strings.ToLower(qname), ".")
	if rest, found := strings.CutSuffix(n, "."+dnsDomain); found {
		n, inDomain = rest, true
		if l, ns, two := strings.Cut(n, "."); two && dnsLabelRE.MatchString(l) && nsRE.MatchString(ns) {
			return l, ns, true, true
		}
	}
	if !dnsLabelRE.MatchString(n) {
		return "", "", inDomain, false
	}
	return n, "", inDomain, true
}

func (d *dnsServer) handler(namespace string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) == 1 {
			q := r.Question[0]
			if label, qualified, inDomain, ok := spawnQuery(q.Name); ok || inDomain {
				var ips []net.IP
				if qualified != "" { // from anywhere: the CGNAT address
					for _, ip := range d.lookup(qualified, label) {
						if c := cgnatAddress(ip.String()); c != nil {
							ips = append(ips, c)
						}
					}
				} else if ok {
					ips = d.lookup(namespace, label)
				}
				if len(ips) > 0 || inDomain {
					m := new(dns.Msg)
					m.SetReply(r)
					m.Authoritative = true
					if len(ips) == 0 {
						m.Rcode = dns.RcodeNameError
					}
					if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
						for _, ip := range ips {
							m.Answer = append(m.Answer, &dns.A{
								Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: dnsTTL}, A: ip})
						}
					}
					_ = w.WriteMsg(m)
					return
				}
			}
		}
		d.forward(w, r)
	}
}

// forward relays a query to the public resolvers, over the protocol it came in on.
func (d *dnsServer) forward(w dns.ResponseWriter, r *dns.Msg) {
	c := &dns.Client{Net: w.LocalAddr().Network(), Timeout: 3 * time.Second}
	if c.Net != "tcp" {
		c.Net = "udp"
	}
	for _, up := range dnsUpstreams {
		resp, _, err := c.Exchange(r, up)
		if err == nil {
			_ = w.WriteMsg(resp)
			return
		}
	}
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}

// lookup finds the running VMs in namespace named label (spawn name or VM id).
func (d *dnsServer) lookup(namespace, label string) []net.IP {
	if label == "" {
		return nil
	}
	var ips []net.IP
	for _, s := range d.states() {
		if namespaceOrDefault(s.Namespace) != namespace || s.IP == "" {
			continue
		}
		if s.Spawn == label || s.ID == label {
			if ip := net.ParseIP(s.IP).To4(); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

// states is the running VMs, read at most once a second.
func (d *dnsServer) states() []state {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.cacheAt) < time.Second {
		return d.cache
	}
	all, err := allStates()
	if err != nil {
		log.Printf("reading VM state: %v", err)
		return d.cache
	}
	var live []state
	for _, s := range all {
		if alive(s) {
			live = append(live, s)
		}
	}
	d.cache, d.cacheAt = live, time.Now()
	return live
}

func netnsOutput(args ...string) (string, error) {
	cmd := osexec.Command(netnsScript, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	return string(out), err
}
