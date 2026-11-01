package cmd

import (
	"io"
	"os"
	"regexp"
	"strings"
	"text/template"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/johndavisdev/persona/internal/ui"
)

// Command groups shown in help.
const (
	groupPersonas = "personas"
	groupBase     = "base"
	groupRun      = "run"
	groupMore     = "more"
)

// colorDisabled reports whether styling is turned off via NO_COLOR (any
// non-empty value, per no-color.org) or the global --no-color flag. Args are
// scanned before cobra parses them so that every writer, including the ones
// used for errors and help, is configured up front.
func colorDisabled(args []string) bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		switch a {
		case "--no-color", "--no-color=true", "--no-color=1":
			return true
		}
	}
	return false
}

// styledOutput wraps f so ANSI styling is downgraded to what the destination
// supports: stripped entirely when disabled or when output is not a terminal.
func styledOutput(f *os.File, noColor bool) io.Writer {
	w := colorprofile.NewWriter(f, os.Environ())
	if noColor {
		w.Profile = colorprofile.NoTTY
	}
	return w
}

var (
	hdrStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1A1A24")).Background(ui.Accent).Padding(0, 1)
	cmdStyle     = lipgloss.NewStyle().Bold(true).Foreground(ui.Accent2)
	flagStyle    = lipgloss.NewStyle().Bold(true).Foreground(ui.Warn)
	argStyle     = lipgloss.NewStyle().Italic(true).Foreground(ui.Muted)
	codeStyle    = lipgloss.NewStyle().Foreground(ui.Accent2)
	cmdRefStyle  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(ui.Accent2)
	commentStyle = lipgloss.NewStyle().Italic(true).Foreground(ui.Muted)
	versionStyle = lipgloss.NewStyle().Foreground(ui.Muted)
)

// render adapts a style to a func(string) string.
func render(st lipgloss.Style) func(string) string {
	return func(s string) string { return st.Render(s) }
}

// gradient renders s with a horizontal color blend across its letters.
func gradient(s string) string {
	runes := []rune(s)
	colors := lipgloss.Blend1D(len(runes), lipgloss.Color("#FF79C6"), ui.Accent, lipgloss.Color("#5FD7FF"))
	var b strings.Builder
	for i, r := range runes {
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colors[i]).Render(string(r)))
	}
	return b.String()
}

func banner(c *cobra.Command) string {
	if !c.HasParent() {
		return gradient("✦ persona") + "  " + versionStyle.Render(c.Root().Version)
	}
	path := strings.Fields(c.CommandPath())
	out := gradient("✦ " + path[0])
	for _, p := range path[1:] {
		out += versionStyle.Render(" › ") + cmdStyle.Render(p)
	}
	return out
}

var (
	backtickRe = regexp.MustCompile("`([^`]+)`")
	cmdRefRe   = regexp.MustCompile(`\bpersona (?:new|list|ls|update|edit-base|edit|delete|rm|build|spawn|join|stop|control-center|base|help)\b`)
	flagRefRe  = regexp.MustCompile(`(^|[\s(/])(--?[a-z][a-z-]*)`)
	envRefRe   = regexp.MustCompile(`\$[A-Z_]+|\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`)
	flagLineRe = regexp.MustCompile(`^(\s+)((?:-\w, )?--[\w-]+)( [\w.\[\]]+)?(\s{2,})(.*)$`)
	defaultRe  = regexp.MustCompile(`\(default .*\)$`)
)

// prose highlights `code`, persona command references, flags, and env vars.
func prose(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		parts := backtickRe.Split(line, -1)
		codes := backtickRe.FindAllStringSubmatch(line, -1)
		var b strings.Builder
		for i, p := range parts {
			p = cmdRefRe.ReplaceAllStringFunc(p, render(cmdRefStyle))
			p = flagRefRe.ReplaceAllString(p, "$1"+flagStyle.Render("$2"))
			p = envRefRe.ReplaceAllStringFunc(p, render(codeStyle))
			b.WriteString(p)
			if i < len(codes) {
				b.WriteString(codeStyle.Render(codes[i][1]))
			}
		}
		out = append(out, b.String())
	}
	return strings.Join(out, "\n")
}

func usageLine(s string) string {
	var out []string
	for _, tok := range strings.Fields(s) {
		if strings.HasPrefix(tok, "[") || strings.HasPrefix(tok, "<") || strings.HasPrefix(tok, `"`) {
			out = append(out, argStyle.Render(tok))
		} else {
			out = append(out, cmdStyle.Render(tok))
		}
	}
	return strings.Join(out, " ")
}

func flagUsages(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		m := flagLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		desc := m[5]
		if d := defaultRe.FindString(desc); d != "" {
			desc = strings.TrimSuffix(desc, d) + argStyle.Render(d)
		}
		lines[i] = m[1] + flagStyle.Render(m[2]) + argStyle.Render(m[3]) + m[4] + desc
	}
	return strings.Join(lines, "\n")
}

func examples(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		indent := l[:len(l)-len(strings.TrimLeft(l, " "))]
		switch {
		case strings.HasPrefix(trimmed, "#"):
			lines[i] = indent + commentStyle.Render(trimmed)
		case strings.HasPrefix(trimmed, "persona"):
			lines[i] = indent + exampleLine(trimmed)
		}
	}
	return strings.Join(lines, "\n")
}

func exampleLine(s string) string {
	var out []string
	inQuote := false
	for i, tok := range strings.Split(s, " ") {
		switch {
		case inQuote || strings.HasPrefix(tok, `"`):
			inQuote = !(strings.HasSuffix(tok, `"`) && (len(tok) > 1 || inQuote))
			out = append(out, codeStyle.Render(tok))
		case i < 2:
			out = append(out, cmdStyle.Render(tok))
		case strings.HasPrefix(tok, "-"):
			out = append(out, flagStyle.Render(tok))
		default:
			out = append(out, tok)
		}
	}
	return strings.Join(out, " ")
}

func aliases(c *cobra.Command) string {
	if len(c.Aliases) == 0 {
		return ""
	}
	return versionStyle.Render(" (" + strings.Join(c.Aliases, ", ") + ")")
}

func footer(c *cobra.Command) string {
	return versionStyle.Render("Run ") + cmdRefStyle.Render(c.CommandPath()+" <command> --help") +
		versionStyle.Render(" for details on a command.")
}

const helpTemplate = `{{banner .}}

{{with (or .Long .Short)}}{{prose . | trimTrailingWhitespaces}}{{end}}

{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

const usageTemplate = `{{hdr "Usage"}}{{if .Runnable}}
  {{usageLine .UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{usageLine (printf "%s [command]" .CommandPath)}}{{end}}{{if gt (len .Aliases) 0}}

{{hdr "Aliases"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{hdr "Examples"}}
{{examples .Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

{{hdr "Commands"}}{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{cmdname (rpad .Name .NamePadding)}} {{.Short}}{{aliases .}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{hdr $group.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{cmdname (rpad .Name .NamePadding)}} {{.Short}}{{aliases .}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

{{hdr "Additional Commands"}}{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{cmdname (rpad .Name .NamePadding)}} {{.Short}}{{aliases .}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{hdr "Flags"}}
{{flags (.LocalFlags.FlagUsages | trimTrailingWhitespaces)}}{{end}}{{if .HasAvailableInheritedFlags}}

{{hdr "Global Flags"}}
{{flags (.InheritedFlags.FlagUsages | trimTrailingWhitespaces)}}{{end}}{{if .HasAvailableSubCommands}}

{{footer .}}{{end}}
`

// styleHelp installs the colorful help and usage templates and command groups.
func styleHelp(root *cobra.Command) {
	funcs := template.FuncMap{
		"banner":    banner,
		"hdr":       func(s string) string { return hdrStyle.Render(strings.ToUpper(s)) },
		"prose":     prose,
		"usageLine": usageLine,
		"cmdname":   render(cmdStyle),
		"flags":     flagUsages,
		"examples":  examples,
		"aliases":   aliases,
		"footer":    footer,
	}
	for name, fn := range funcs {
		cobra.AddTemplateFunc(name, fn)
	}
	root.SetHelpTemplate(helpTemplate)
	root.SetUsageTemplate(usageTemplate)
	root.SetVersionTemplate(`{{banner .}}` + "\n")

	root.AddGroup(
		&cobra.Group{ID: groupPersonas, Title: "Personas"},
		&cobra.Group{ID: groupRun, Title: "Spawns"},
		&cobra.Group{ID: groupBase, Title: "Shared base"},
		&cobra.Group{ID: groupMore, Title: "More"},
	)
	groups := map[string]string{
		"new": groupPersonas, "list": groupPersonas, "update": groupPersonas, "edit": groupPersonas,
		"delete": groupPersonas, "build": groupPersonas,
		"spawn": groupRun, "ps": groupRun, "attach": groupRun, "stop": groupRun, "start": groupRun,
		"save": groupRun, "kill": groupRun, "control-center": groupRun, "manage": groupRun,
		"base": groupBase, "edit-base": groupBase,
		"configure": groupMore, "auth": groupMore,
	}
	for _, c := range root.Commands() {
		if g, ok := groups[c.Name()]; ok {
			c.GroupID = g
		}
	}
	root.SetHelpCommandGroupID(groupMore)
	root.SetCompletionCommandGroupID(groupMore)
}
