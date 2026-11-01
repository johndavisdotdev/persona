//go:build linux

package instance

import (
	"context"
	"fmt"

	"github.com/johndavisdev/persona/internal/microvm"
)

var _ Forwarder = (*VM)(nil)

func (v *VM) AddForward(ctx context.Context, ref, proto, hostIP string, hostPort, port int) (PortForward, error) {
	cs, err := Resolve(ctx, v, ref)
	if err != nil {
		return PortForward{}, err
	}
	if len(cs) != 1 {
		return PortForward{}, fmt.Errorf("%q matches %d spawns; use a spawn's name", ref, len(cs))
	}
	c := cs[0]
	if !c.Running() {
		return PortForward{}, fmt.Errorf("%s is not running; start it first (persona start %s)", c.Instance, c.Instance)
	}
	f, err := microvm.AddForward(ctx, c.ID, proto, hostIP, hostPort, port)
	if err != nil {
		return PortForward{}, err
	}
	return v.portForward(f, map[string]Container{c.ID: c}), nil
}

func (v *VM) Forwards(ctx context.Context) ([]PortForward, error) {
	fs, err := microvm.Forwards(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := v.List(ctx, "")
	if err != nil {
		return nil, err
	}
	byVM := map[string]Container{}
	for _, c := range cs {
		byVM[c.ID] = c
	}
	var out []PortForward
	for _, f := range fs {
		out = append(out, v.portForward(f, byVM))
	}
	return out, nil
}

func (v *VM) RemoveForward(ctx context.Context, id int) error { return microvm.RemoveForward(ctx, id) }

func (v *VM) portForward(f microvm.Forward, byVM map[string]Container) PortForward {
	p := PortForward{ID: f.ID, Spawn: f.VM, Namespace: f.Namespace, Proto: f.Proto, HostIP: f.HostIP,
		HostPort: f.HostPort, Port: f.VMPort}
	if c, ok := byVM[f.VM]; ok {
		p.Spawn, p.Persona = c.Instance, c.Persona
	}
	return p
}
