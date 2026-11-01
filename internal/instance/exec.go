//go:build linux

package instance

import (
	"context"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/johndavisdev/persona/internal/microvm"
)

var _ Commander = (*VM)(nil)

// RunCommand runs argv in the VM through its agent: as the session user, in
// the session's directory, with the image's environment (not the session's
// secrets, which only Claude Code's own session sees).
func (v *VM) RunCommand(ctx context.Context, ref string, argv []string, stdin []byte) (ExecResult, error) {
	c, err := resolveRunning(ctx, v, ref)
	if err != nil {
		return ExecResult{}, err
	}
	rec, err := loadRecord(c.Name)
	if err != nil {
		return ExecResult{}, err
	}
	vm, ok := microvm.Lookup(rec.VMID)
	if !ok {
		return ExecResult{}, ErrNoMatch
	}
	resp, err := vm.Agent.Do(ctx, agent.Request{Op: "exec", Exec: &agent.Exec{
		Argv: argv, User: rec.User, Env: rec.Env, Dir: rec.Cwd, Stdin: stdin}})
	if !resp.OK && resp.Error != "" { // the agent answered: the command couldn't start
		return ExecResult{}, &ErrNotStarted{Msg: resp.Error}
	}
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Stdout: resp.Stdout, Stderr: resp.Stderr, Exit: resp.Exit}, nil
}
