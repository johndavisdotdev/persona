//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Port forwards expose a VM's port on a host address: 127.0.0.1, or one of
// the host's own addresses; or, to other spawns in every namespace, on the
// VM's own CGNAT address (--host-ip cgnat; see cgnatAddress). They are kept in forwardsPath, one per line, and
// applied by persona-netns (which documents the iptables side). A forward
// belongs to the user who made it, and can only point at that user's own
// running VM; it lasts until removed, or until the VM's spawn is killed.

const (
	forwardsPath = "/var/lib/persona/forwards"
	// Rule ids also pick the namespace-side port (20000+id): keep it a port.
	maxForwardID = 45535
)

var (
	vmSubnet  = &net.IPNet{IP: net.IPv4(10, 200, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}
	vethRange = &net.IPNet{IP: net.IPv4(10, 199, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}
	// cgnatRange holds the addresses spawns expose ports on to other spawns,
	// in any namespace: VM 10.200.a.b is 100.64.a.b.
	cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}
)

// cgnatAddress is the address VM vmIP's ports are exposed on to other spawns.
func cgnatAddress(vmIP string) net.IP {
	ip := net.ParseIP(vmIP).To4()
	if ip == nil || !vmSubnet.Contains(ip) {
		return nil
	}
	return net.IPv4(100, 64, ip[2], ip[3]).To4()
}

// Forward is one port forward, as fw-add and fw-list print it.
type Forward struct {
	ID        int    `json:"id"`
	Owner     int    `json:"owner"`
	VM        string `json:"vm"`
	Namespace string `json:"namespace"`
	Proto     string `json:"proto"`
	HostIP    string `json:"host_ip"`
	HostPort  int    `json:"host_port"`
	VMIP      string `json:"vm_ip"`
	VMPort    int    `json:"vm_port"`
}

func (f Forward) line() string {
	return fmt.Sprintf("%d %d %s %s %s %s %d %s %d", f.ID, f.Owner, f.VM, f.Namespace, f.Proto, f.HostIP, f.HostPort, f.VMIP, f.VMPort)
}

func readForwards() ([]Forward, error) {
	f, err := os.Open(forwardsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Forward
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var fw Forward
		if _, err := fmt.Sscanf(sc.Text(), "%d %d %s %s %s %s %d %s %d", &fw.ID, &fw.Owner, &fw.VM, &fw.Namespace,
			&fw.Proto, &fw.HostIP, &fw.HostPort, &fw.VMIP, &fw.VMPort); err == nil {
			out = append(out, fw)
		}
	}
	return out, sc.Err()
}

func writeForwards(fws []Forward) error {
	sort.Slice(fws, func(i, j int) bool { return fws[i].ID < fws[j].ID })
	var b strings.Builder
	for _, f := range fws {
		b.WriteString(f.line() + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(forwardsPath), 0o755); err != nil {
		return err
	}
	tmp := forwardsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, forwardsPath)
}

// saveAndApply writes fws and applies them; if applying fails, it puts the
// old rules back.
func saveAndApply(fws, old []Forward) error {
	if err := writeForwards(fws); err != nil {
		return err
	}
	if err := netns("ports"); err != nil {
		_ = writeForwards(old)
		_ = netns("ports")
		return fmt.Errorf("applying the port forwards: %w", err)
	}
	return nil
}

// hostAddress checks that ip is 127.0.0.1 or one of this host's own IPv4
// addresses (but not the namespaces' veth addresses).
func hostAddress(ip net.IP) error {
	if ip == nil || ip.To4() == nil {
		return errors.New("the host address must be IPv4")
	}
	if ip.Equal(net.IPv4(127, 0, 0, 1)) {
		return nil
	}
	if vethRange.Contains(ip) || vmSubnet.Contains(ip) {
		return fmt.Errorf("%s is persona's own network, not a host address", ip)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return nil
		}
	}
	return fmt.Errorf("%s is not an address of this host (use localhost or one of the host's IPs)", ip)
}

func cmdFWAdd(args []string) error {
	owner, _, err := caller()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("fw-add", flag.ContinueOnError)
	id := fs.String("id", "", "VM id")
	proto := fs.String("proto", "tcp", "tcp or udp")
	hostIP := fs.String("host-ip", "", "host address to listen on")
	hostPort := fs.Int("host-port", 0, "host port")
	vmPort := fs.Int("port", 0, "VM port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *proto != "tcp" && *proto != "udp" {
		return fmt.Errorf("protocol must be tcp or udp, not %q", *proto)
	}
	for _, p := range []int{*hostPort, *vmPort} {
		if p < 1 || p > 65535 {
			return fmt.Errorf("invalid port %d", p)
		}
	}
	cgnat := *hostIP == "cgnat" || cgnatRange.Contains(net.ParseIP(*hostIP))
	ip := net.ParseIP(*hostIP)
	if !cgnat {
		if err := hostAddress(ip); err != nil {
			return err
		}
	}

	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()
	s, err := readState(*id)
	if err != nil || s.Owner != owner || !alive(s) {
		return fmt.Errorf("no running microVM %q of yours (start the spawn first)", *id)
	}
	if s.IP == "" {
		return fmt.Errorf("%s was started before port forwards existed; restart it (persona stop / start)", *id)
	}
	if cgnat {
		// Only the VM's own address: one can't take over another's.
		own := cgnatAddress(s.IP)
		if own == nil {
			return fmt.Errorf("%s has no CGNAT address", *id)
		}
		if *hostIP != "cgnat" && !ip.Equal(own) {
			return fmt.Errorf("%s is not %s's address; its ports are exposed to spawns on %s (or say cgnat)", *hostIP, *id, own)
		}
		ip = own
	}
	fws, err := readForwards()
	if err != nil {
		return err
	}
	next := 1
	for _, f := range fws {
		if f.Proto == *proto && f.HostPort == *hostPort && f.HostIP == ip.String() {
			return fmt.Errorf("%s:%d/%s is already forwarded (rule %d)", f.HostIP, f.HostPort, f.Proto, f.ID)
		}
		next = max(next, f.ID+1)
	}
	if next > maxForwardID {
		return errors.New("too many port forwards")
	}
	fw := Forward{ID: next, Owner: owner, VM: s.ID, Namespace: namespaceOrDefault(s.Namespace), Proto: *proto,
		HostIP: ip.String(), HostPort: *hostPort, VMIP: s.IP, VMPort: *vmPort}
	if err := saveAndApply(append(append([]Forward{}, fws...), fw), fws); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(fw)
}

// cmdFWRemove removes one of the caller's forwards by id, or (--vm) all of
// the caller's forwards to one VM.
func cmdFWRemove(args []string) error {
	owner, _, err := caller()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("fw-rm", flag.ContinueOnError)
	vm := fs.String("vm", "", "remove every forward to this VM id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ruleID := 0
	switch {
	case *vm != "" && fs.NArg() == 0:
		if !idRE.MatchString(*vm) {
			return fmt.Errorf("invalid --vm %q", *vm)
		}
	case *vm == "" && fs.NArg() == 1:
		if ruleID, err = strconv.Atoi(fs.Arg(0)); err != nil || ruleID < 1 {
			return fmt.Errorf("invalid rule id %q", fs.Arg(0))
		}
	default:
		usage()
	}
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()
	fws, err := readForwards()
	if err != nil {
		return err
	}
	var keep []Forward
	removed := 0
	for _, f := range fws {
		mine := f.Owner == owner || owner == 0
		if mine && (f.ID == ruleID || *vm != "" && f.VM == *vm) {
			removed++
			continue
		}
		if f.ID == ruleID {
			return fmt.Errorf("rule %d isn't yours", ruleID)
		}
		keep = append(keep, f)
	}
	if removed == 0 {
		if ruleID != 0 {
			return fmt.Errorf("no rule %d", ruleID)
		}
		return nil // no forwards to that VM
	}
	return saveAndApply(keep, fws)
}

// cmdFWList prints the caller's forwards, one JSON object per line.
func cmdFWList(args []string) error {
	if len(args) != 0 {
		usage()
	}
	owner, _, err := caller()
	if err != nil {
		return err
	}
	fws, err := readForwards()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, f := range fws {
		if f.Owner == owner {
			if err := enc.Encode(f); err != nil {
				return err
			}
		}
	}
	return nil
}
