//go:build !linux

package instance

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"time"
)

var errLinuxOnly = errors.New("the firecracker runtime needs Linux with KVM; set runtime: docker")

func firecrackerRuntime() Runtime { return unsupported{} }

// unsupported stands in for the firecracker runtime off Linux.
type unsupported struct{}

func (unsupported) Name() string                                    { return "firecracker" }
func (unsupported) Available(context.Context) error                 { return errLinuxOnly }
func (unsupported) Run(context.Context, RunOptions) (string, error) { return "", errLinuxOnly }
func (unsupported) List(context.Context, string) ([]Container, error) {
	return nil, errLinuxOnly
}
func (unsupported) Stop(context.Context, string) error   { return errLinuxOnly }
func (unsupported) Remove(context.Context, string) error { return errLinuxOnly }
func (unsupported) Exec(context.Context, string, ...string) ([]byte, error) {
	return nil, errLinuxOnly
}
func (unsupported) AttachCommand(ctx context.Context, _ string) *exec.Cmd {
	return exec.CommandContext(ctx, "false")
}

// Broker serves microVM instances, which only exist on Linux.
type Broker struct {
	GH   string
	Log  *log.Logger
	Idle time.Duration
}

func (*Broker) Run(context.Context) error { return errLinuxOnly }

// AttachMain is Linux-only.
func AttachMain(context.Context, string) error { return errLinuxOnly }

// RshMain is Linux-only.
func RshMain([]string) int { return 1 }
