//go:build linux

package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
)

// Forward is a port forward from a host address to a VM (persona-vm fw-add).
type Forward struct {
	ID        int    `json:"id"`
	VM        string `json:"vm"` // VM id
	Namespace string `json:"namespace"`
	Proto     string `json:"proto"` // tcp or udp
	HostIP    string `json:"host_ip"`
	HostPort  int    `json:"host_port"`
	VMIP      string `json:"vm_ip"`
	VMPort    int    `json:"vm_port"`
}

// AddForward forwards hostIP:hostPort (proto) to port of the running VM id.
func AddForward(ctx context.Context, id, proto, hostIP string, hostPort, port int) (Forward, error) {
	var out bytes.Buffer
	if err := nsWrapper(ctx, &out, "fw-add", "--id", id, "--proto", proto, "--host-ip", hostIP,
		"--host-port", strconv.Itoa(hostPort), "--port", strconv.Itoa(port)); err != nil {
		return Forward{}, err
	}
	var f Forward
	err := json.Unmarshal(out.Bytes(), &f)
	return f, err
}

// RemoveForward removes one of this user's forwards.
func RemoveForward(ctx context.Context, ruleID int) error {
	return nsWrapper(ctx, nil, "fw-rm", strconv.Itoa(ruleID))
}

// RemoveForwardsTo removes this user's forwards to VM id.
func RemoveForwardsTo(ctx context.Context, id string) error {
	return nsWrapper(ctx, nil, "fw-rm", "--vm", id)
}

// Forwards lists this user's forwards.
func Forwards(ctx context.Context) ([]Forward, error) {
	var out bytes.Buffer
	if err := nsWrapper(ctx, &out, "fw-list"); err != nil {
		return nil, err
	}
	var fs []Forward
	dec := json.NewDecoder(&out)
	for dec.More() {
		var f Forward
		if err := dec.Decode(&f); err != nil {
			return nil, err
		}
		fs = append(fs, f)
	}
	return fs, nil
}
