// Package manage is `persona manage`: a dashboard of every spawn driven by
// the status stream (see package status) rather than by mirroring their
// terminals. It shows what each agent is doing, surfaces questions and
// permission prompts, and types replies into the agent's session.
package manage

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/status"
	"github.com/johndavisdev/persona/internal/ui"
)

const (
	tickEvery = time.Second
	listEvery = 3 * time.Second
	keepLast  = 300 // events kept per spawn
)

type spawnRow struct {
	c       instance.Container
	off     int64
	events  []status.Event
	state   status.State
	pending *status.Event
}

type model struct {
	ctx      context.Context
	rt       instance.Runtime
	rows     []*spawnRow
	sel      int
	width    int
	height   int
	replying bool
	input    textinput.Model
	note     string
	noteErr  bool
	lastList time.Time
}

type (
	tickMsg struct{}
	listMsg struct{ cs []instance.Container }
	sentMsg struct {
		spawn string
		err   error
	}
	errMsg   struct{ err error }
	doneExec struct{ err error }
)

// Run starts the dashboard.
func Run(ctx context.Context, rt instance.Runtime) error {
	in := textinput.New()
	in.Prompt = "› "
	in.CharLimit = 4000
	m := &model{ctx: ctx, rt: rt, input: in}
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.list(), tick()) }

func tick() tea.Cmd { return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m *model) list() tea.Cmd {
	m.lastList = time.Now()
	return func() tea.Msg {
		cs, err := m.rt.List(m.ctx, "")
		if err != nil {
			return errMsg{err}
		}
		return listMsg{cs}
	}
}

// readEvents loads new status events and reports spawns that just started
// needing input.
func (m *model) readEvents() (alert bool) {
	for _, r := range m.rows {
		evs, off, err := status.Read(r.c.Instance, r.off)
		if err != nil || len(evs) == 0 {
			r.off = off
			continue
		}
		r.off = off
		r.events = append(r.events, evs...)
		if len(r.events) > keepLast {
			r.events = r.events[len(r.events)-keepLast:]
		}
		before := r.state
		r.state, r.pending = status.Derive(r.events)
		if r.state == status.NeedsInput && before != status.NeedsInput {
			alert = true
		}
	}
	return alert
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(max(10, msg.Width-6))
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{tick()}
		if time.Since(m.lastList) > listEvery {
			cmds = append(cmds, m.list())
		}
		if m.readEvents() {
			cmds = append(cmds, bell)
		}
		return m, tea.Batch(cmds...)
	case listMsg:
		m.merge(msg.cs)
		m.readEvents()
		return m, nil
	case errMsg:
		m.setNote(true, "%v", msg.err)
		return m, nil
	case sentMsg:
		if msg.err != nil {
			m.setNote(true, "reply to %s: %v", msg.spawn, msg.err)
		} else {
			m.setNote(false, "Sent to %s", msg.spawn)
		}
		return m, nil
	case doneExec:
		if msg.err != nil {
			m.setNote(true, "attach: %v", msg.err)
		}
		return m, m.list()
	case tea.KeyPressMsg:
		if m.replying {
			return m, m.replyKey(msg)
		}
		return m, m.key(msg)
	}
	if m.replying {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// merge updates the spawn list, keeping each spawn's read position.
func (m *model) merge(cs []instance.Container) {
	old := map[string]*spawnRow{}
	for _, r := range m.rows {
		old[r.c.Instance] = r
	}
	var rows []*spawnRow
	for _, c := range cs {
		r := old[c.Instance]
		if r == nil {
			r = &spawnRow{state: status.Unknown}
		}
		r.c = c
		rows = append(rows, r)
	}
	m.rows = rows
	m.sel = min(m.sel, max(0, len(rows)-1))
}

func (m *model) selected() *spawnRow {
	if m.sel < 0 || m.sel >= len(m.rows) {
		return nil
	}
	return m.rows[m.sel]
}

func (m *model) setNote(isErr bool, format string, a ...any) {
	m.note, m.noteErr = fmt.Sprintf(format, a...), isErr
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "q", "ctrl+c":
		return tea.Quit
	case "up", "k":
		m.sel = max(0, m.sel-1)
	case "down", "j":
		m.sel = min(len(m.rows)-1, m.sel+1)
	case "enter", "r":
		r := m.selected()
		if r == nil {
			return nil
		}
		if !r.c.Running() {
			m.setNote(true, "%s is stopped; start it with `persona start %s`", r.c.Instance, r.c.Instance)
			return nil
		}
		m.replying = true
		m.input.Placeholder = "reply to " + r.c.Instance + " (enter sends, esc cancels)"
		m.input.SetValue("")
		return m.input.Focus()
	case "a":
		r := m.selected()
		if r == nil || !r.c.Running() {
			return nil
		}
		return tea.ExecProcess(m.rt.AttachCommand(m.ctx, r.c.Name), func(err error) tea.Msg { return doneExec{err} })
	}
	return nil
}

func (m *model) replyKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.replying = false
		m.input.Blur()
		return nil
	case "enter":
		text := m.input.Value()
		m.replying = false
		m.input.Blur()
		r := m.selected()
		if r == nil || strings.TrimSpace(text) == "" {
			return nil
		}
		return m.send(r.c, text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// send types text into the spawn's agent session and presses Enter.
func (m *model) send(c instance.Container, text string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		_, err := m.rt.Exec(ctx, c.Name, "tmux", "send-keys", "-t", persona.SessionName, "-l", "--", text)
		if err == nil {
			_, err = m.rt.Exec(ctx, c.Name, "tmux", "send-keys", "-t", persona.SessionName, "Enter")
		}
		return sentMsg{spawn: c.Instance, err: err}
	}
}

func bell() tea.Msg {
	os.Stdout.WriteString("\a")
	return nil
}

// ---- view ----

var (
	stWorking = lipgloss.NewStyle().Foreground(ui.Accent2)
	stNeeds   = lipgloss.NewStyle().Foreground(ui.Warn).Bold(true)
	stIdle    = lipgloss.NewStyle().Foreground(ui.Accent)
	stOff     = lipgloss.NewStyle().Foreground(ui.Muted)
	stFailed  = lipgloss.NewStyle().Foreground(ui.Danger)
	selRow    = lipgloss.NewStyle().Background(ui.Subtle)
)

func stateStyle(r *spawnRow) (lipgloss.Style, string) {
	if !r.c.Running() {
		return stOff, "stopped"
	}
	switch r.state {
	case status.Working:
		return stWorking, "working"
	case status.NeedsInput:
		return stNeeds, "needs input"
	case status.Idle:
		return stIdle, "idle"
	case status.Failed:
		return stFailed, "failed"
	}
	return stOff, "running"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.WindowTitle = "persona · manage"
	if m.width == 0 {
		return v
	}
	w := m.width
	var b strings.Builder

	needs := 0
	for _, r := range m.rows {
		if r.c.Running() && r.state == status.NeedsInput {
			needs++
		}
	}
	head := ui.Title.Render("persona manage") + ui.Dim.Render(fmt.Sprintf(" · %d spawns", len(m.rows)))
	if needs > 0 {
		head += stNeeds.Render(fmt.Sprintf(" · %d need input", needs))
	}
	b.WriteString(head + "\n\n")

	if len(m.rows) == 0 {
		b.WriteString(ui.Dim.Render("No spawns. Start one with `persona spawn <persona>`.") + "\n")
	}
	nameW, persW := 6, 7
	for _, r := range m.rows {
		nameW = max(nameW, len(r.c.Instance))
		persW = max(persW, len(r.c.Persona))
	}
	listH := min(len(m.rows), max(3, (m.height-8)/2))
	start := 0
	if m.sel >= listH {
		start = m.sel - listH + 1
	}
	for i := start; i < len(m.rows) && i < start+listH; i++ {
		r := m.rows[i]
		st, label := stateStyle(r)
		last, when := "", ""
		if n := len(r.events); n > 0 {
			last, when = r.events[n-1].Summary(), ago(r.events[n-1].TS)
		}
		line := fmt.Sprintf("%s %-*s  %-*s  %s  %4s  ", st.Render("●"), nameW, r.c.Instance, persW, r.c.Persona,
			st.Render(fmt.Sprintf("%-11s", label)), when)
		line += ui.Dim.Render(ansi.Truncate(last, max(0, w-lipgloss.Width(line)-1), "…"))
		if i == m.sel {
			line = selRow.Render(ansi.Truncate(line, w, "") + strings.Repeat(" ", max(0, w-lipgloss.Width(line))))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(ui.Dim.Render(strings.Repeat("─", w)) + "\n")

	used := strings.Count(b.String(), "\n")
	if r := m.selected(); r != nil {
		st, label := stateStyle(r)
		b.WriteString(ui.Bold.Render(r.c.Instance) + ui.Dim.Render(" · "+r.c.Persona+" · ") + st.Render(label) +
			ui.Dim.Render(" · "+r.c.Status) + "\n")
		used++
		if r.pending != nil && r.c.Running() {
			for _, l := range pendingLines(r.pending, sender(r.c), w) {
				b.WriteString(l + "\n")
				used++
			}
		}
		room := m.height - used - 3
		evs := r.events
		if len(evs) > room {
			evs = evs[len(evs)-max(0, room):]
		}
		if len(r.events) == 0 {
			b.WriteString(ui.Dim.Render("  no status events yet") + "\n")
		}
		for _, ev := range evs {
			line := ui.Dim.Render(ev.TS.Local().Format("15:04:05")+" ") + sender(r.c) + " " + ev.Summary()
			b.WriteString(ansi.Truncate(line, w, "…") + "\n")
		}
	}

	// Pin the footer to the bottom.
	content := strings.TrimRight(b.String(), "\n")
	lines := strings.Split(content, "\n")
	for len(lines) < m.height-2 {
		lines = append(lines, "")
	}
	if len(lines) > m.height-2 {
		lines = lines[:m.height-2]
	}
	foot := ui.Dim.Render("↑↓ select · enter reply · a attach · q quit")
	if m.note != "" {
		style := ui.Dim
		if m.noteErr {
			style = ui.ErrS
		}
		foot = style.Render(m.note) + ui.Dim.Render("  ·  ") + foot
	}
	if m.replying {
		foot = m.input.View()
	}
	lines = append(lines, "", ansi.Truncate(foot, w, "…"))
	v.Content = strings.Join(lines, "\n")
	return v
}

// sender names who a message is from: the spawn, then its persona.
func sender(c instance.Container) string {
	return ui.Bold.Render(c.Instance) + ui.Dim.Render(" ("+c.Persona+"):")
}

// pendingLines render what the agent is waiting on: a question with its
// numbered options, or a permission/input notification.
func pendingLines(ev *status.Event, from string, w int) []string {
	var out []string
	if len(ev.Questions) == 0 {
		return []string{ansi.Truncate(stNeeds.Render("  ! ")+from+" "+strings.Join(strings.Fields(ev.Message), " "), w, "…")}
	}
	for _, q := range ev.Questions {
		out = append(out, ansi.Truncate(stNeeds.Render("  ? ")+from+" "+ui.Bold.Render(q.Question), w, "…"))
		for i, o := range q.Options {
			opt := fmt.Sprintf("    %d. %s", i+1, o.Label)
			if o.Description != "" {
				opt += ui.Dim.Render(" — " + o.Description)
			}
			out = append(out, ansi.Truncate(opt, w, "…"))
		}
	}
	if len(ev.Questions[0].Options) > 0 {
		out = append(out, ui.Dim.Render("    reply with an option's number, or your own answer"))
	}
	return out
}
