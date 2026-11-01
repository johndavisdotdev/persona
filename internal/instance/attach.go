//go:build linux

package instance

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/johndavisdev/persona/internal/persona"
)

// AttachMain attaches this terminal to a VM instance's Claude Code session
// (tmux attach over a pty stream). Detach with the usual tmux keys.
func AttachMain(ctx context.Context, name string) error {
	v := NewVM()
	rec, vm, err := v.lookup(name)
	if err != nil {
		return err
	}
	in := os.Stdin.Fd()
	if !term.IsTerminal(in) {
		return errors.New("attaching requires an interactive terminal")
	}
	cols, rows, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		cols, rows = 200, 50
	}
	conn, session, err := vm.Agent.Pty(ctx, agent.Exec{
		Argv: []string{"tmux", "-u", "attach-session", "-t", persona.SessionName},
		User: rec.User, Env: append(append([]string{}, rec.Env...), "TERM=xterm-256color"), Dir: rec.Cwd,
		Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return err
	}
	defer conn.Close()
	state, err := term.MakeRaw(in)
	if err != nil {
		return err
	}
	defer term.Restore(in, state)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if c, r, err := term.GetSize(os.Stdout.Fd()); err == nil {
				_ = vm.Agent.Resize(ctx, session, uint16(r), uint16(c))
			}
		}
	}()
	go io.Copy(conn, os.Stdin)
	_, err = io.Copy(os.Stdout, conn)
	return err
}
