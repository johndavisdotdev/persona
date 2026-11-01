// Package ui holds shared terminal styling and small interaction helpers.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"

	"github.com/johndavisdev/persona/internal/persona"
)

// Palette.
var (
	Accent  = lipgloss.Color("#B48EFF")
	Accent2 = lipgloss.Color("#5FD7AF")
	Muted   = lipgloss.Color("#7C7C8A")
	Warn    = lipgloss.Color("#FFB86C")
	Danger  = lipgloss.Color("#FF6E6E")
	Subtle  = lipgloss.Color("#3A3A4A")
)

// Styles.
var (
	Title   = lipgloss.NewStyle().Bold(true).Foreground(Accent)
	Heading = lipgloss.NewStyle().Bold(true).Foreground(Accent2)
	Dim     = lipgloss.NewStyle().Foreground(Muted)
	Bold    = lipgloss.NewStyle().Bold(true)
	OK      = lipgloss.NewStyle().Foreground(Accent2)
	WarnS   = lipgloss.NewStyle().Foreground(Warn)
	ErrS    = lipgloss.NewStyle().Foreground(Danger).Bold(true)
	Box     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Subtle).Padding(0, 1)
)

// Interactive reports whether stdin and stdout are terminals.
func Interactive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// Step prints a progress line.
func Step(w io.Writer, msg string) {
	if strings.HasPrefix(msg, "warning:") || strings.HasPrefix(msg, "Rejected") {
		fmt.Fprintln(w, WarnS.Render("  ! "+msg))
		return
	}
	fmt.Fprintln(w, Dim.Render("  • ")+msg)
}

// Success prints a success line.
func Success(w io.Writer, msg string) { fmt.Fprintln(w, OK.Render("✓ ")+msg) }

// Warning prints a warning line.
func Warning(w io.Writer, msg string) { fmt.Fprintln(w, WarnS.Render("! "+msg)) }

// RenderPersona renders a human-readable summary of a persona.
func RenderPersona(p *persona.Persona) string {
	var b strings.Builder
	b.WriteString(Title.Render(p.Name))
	if p.Model != "" {
		b.WriteString(Dim.Render("  model: " + p.Model))
	}
	b.WriteString("\n")
	if p.Summary != "" {
		b.WriteString(p.Summary + "\n")
	}
	section := func(name string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("\n" + Heading.Render(name) + "\n")
		for _, it := range items {
			b.WriteString("  " + it + "\n")
		}
	}
	var pl, mcp, sk []string
	for _, x := range p.Plugins {
		pl = append(pl, Bold.Render(x.ID())+reason(x.Reason))
	}
	for _, x := range p.MCPServers {
		target := x.URL
		if x.Type == "stdio" {
			target = strings.TrimSpace(x.Command + " " + strings.Join(x.Args, " "))
		}
		mcp = append(mcp, Bold.Render(x.Name)+Dim.Render(" ["+x.Type+"] "+target)+reason(x.Reason))
	}
	for _, x := range p.Skills {
		sk = append(sk, Bold.Render(x.Name)+Dim.Render(" "+x.Repo+"/"+x.Path)+reason(x.Reason))
	}
	section("Plugins", pl)
	section("MCP servers", mcp)
	section("Skills", sk)
	section("Extra packages", p.AptPackages)
	section("Forwarded env", p.Env)
	return Box.Render(strings.TrimRight(b.String(), "\n"))
}

func reason(r string) string {
	if r == "" {
		return ""
	}
	return Dim.Render(" — " + r)
}
