// Package tui implements the persona control center: a Bubble Tea app that
// multiplexes running persona containers in split panes and tabs.
package tui

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/harmonica"
	"github.com/charmbracelet/x/ansi"

	"github.com/johndavisdev/persona/internal/docker"
	"github.com/johndavisdev/persona/internal/persona"
	"github.com/johndavisdev/persona/internal/ui"
)

// SpawnFunc starts a container for a persona sized cols x rows and returns its name.
type SpawnFunc func(ctx context.Context, persona string, cols, rows int) (string, error)

const (
	sidebarOpenWidth = 30
	pollEvery        = 150 * time.Millisecond
	refreshEvery     = 2 * time.Second
)

type mode int

const (
	modeNormal mode = iota
	modeInsert
)

type tab struct {
	panes    []*pane
	focus    int
	vertical bool
}

type sideItem struct {
	persona   string
	container string // empty for a persona row
	state     string
	summary   string
}

type model struct {
	ctx   context.Context
	spawn SpawnFunc

	width, height int
	mode          mode
	showHelp      bool

	personas   []*persona.Persona
	containers []docker.Container
	items      []sideItem
	cursor     int
	seeded     bool

	tabs   []*tab
	active int
	nextID int

	sidebarOpen bool
	sidebarX    float64
	sidebarV    float64
	spring      harmonica.Spring
	animating   bool

	busy    int
	spin    spinner.Model
	status  string
	isError bool
	help    help.Model

	rects map[int]rect // pane id -> screen rect (from last layout)
}

// Run starts the control center.
func Run(ctx context.Context, spawn SpawnFunc) error {
	m := &model{
		ctx:         ctx,
		spawn:       spawn,
		sidebarOpen: true,
		sidebarX:    sidebarOpenWidth,
		spring:      harmonica.NewSpring(harmonica.FPS(60), 7.0, 0.85),
		spin:        spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(ui.Accent))),
		help:        help.New(),
		rects:       map[int]rect{},
	}
	m.tabs = []*tab{{panes: []*pane{m.newPane()}}}
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	for _, t := range m.tabs {
		for _, p := range t.panes {
			p.detach()
		}
	}
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

// ---- messages ----

type pollMsg struct{}
type refreshMsg struct{}
type frameMsg struct{}

type dataMsg struct {
	personas   []*persona.Persona
	containers []docker.Container
	err        error
}

type spawnedMsg struct {
	pane      int
	persona   string
	container string
	err       error
}

type stoppedMsg struct {
	container string
	err       error
}

type attachedMsg struct{ err error }

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.load(), m.poll(), tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshMsg{} }))
}

func (m *model) poll() tea.Cmd {
	return tea.Tick(pollEvery, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m *model) frame() tea.Cmd {
	return tea.Tick(time.Second/60, func(time.Time) tea.Msg { return frameMsg{} })
}

func (m *model) load() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		ps, _ := persona.List()
		cs, err := docker.List(ctx, "")
		return dataMsg{personas: ps, containers: cs, err: err}
	}
}

// ---- helpers ----

func (m *model) newPane() *pane {
	m.nextID++
	return &pane{id: m.nextID}
}

func (m *model) tab() *tab { return m.tabs[m.active] }

func (m *model) focused() *pane {
	t := m.tab()
	if len(t.panes) == 0 {
		return nil
	}
	return t.panes[t.focus]
}

func (m *model) findPane(id int) *pane {
	for _, t := range m.tabs {
		for _, p := range t.panes {
			if p.id == id {
				return p
			}
		}
	}
	return nil
}

func (m *model) setStatus(err bool, format string, a ...any) {
	m.status, m.isError = fmt.Sprintf(format, a...), err
}

func (m *model) sidebarWidth() int { return int(math.Round(math.Max(0, m.sidebarX))) }

func (m *model) rebuildItems() {
	var items []sideItem
	byPersona := map[string][]docker.Container{}
	for _, c := range m.containers {
		byPersona[c.Persona] = append(byPersona[c.Persona], c)
	}
	known := map[string]bool{}
	for _, p := range m.personas {
		known[p.Name] = true
		items = append(items, sideItem{persona: p.Name, summary: p.Summary})
		for _, c := range byPersona[p.Name] {
			items = append(items, sideItem{persona: p.Name, container: c.Name, state: c.State})
		}
	}
	for name, cs := range byPersona {
		if known[name] {
			continue
		}
		for _, c := range cs {
			items = append(items, sideItem{persona: name, container: c.Name, state: c.State})
		}
	}
	m.items = items
	if m.cursor >= len(items) {
		m.cursor = max(0, len(items)-1)
	}
}

func (m *model) containerRunning(name string) bool {
	for _, c := range m.containers {
		if c.Name == name {
			return c.Running()
		}
	}
	return false
}

// openContainer shows a container in the focused pane if it is empty,
// otherwise in a new pane in the current tab.
func (m *model) openContainer(container, personaName string) {
	t := m.tab()
	for i, p := range t.panes {
		if p.container == container {
			t.focus = i
			return
		}
	}
	target := m.focused()
	if target == nil || target.container != "" || target.starting {
		target = m.newPane()
		t.panes = append(t.panes, target)
		t.focus = len(t.panes) - 1
	}
	target.attach(m.ctx, container, personaName)
}

func (m *model) spawnInto(personaName string, forceNew bool) tea.Cmd {
	t := m.tab()
	target := m.focused()
	if forceNew || target == nil || target.container != "" || target.starting {
		target = m.newPane()
		t.panes = append(t.panes, target)
		t.focus = len(t.panes) - 1
	}
	target.starting, target.persona = true, personaName
	m.layout()
	r := m.rects[target.id]
	cols, rows := max(20, r.w-2), max(5, r.h-2)
	m.busy++
	m.setStatus(false, "Spawning %s (building the image first if needed)…", personaName)
	id, ctx, spawn := target.id, m.ctx, m.spawn
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		name, err := spawn(ctx, personaName, cols, rows)
		return spawnedMsg{pane: id, persona: personaName, container: name, err: err}
	})
}

func (m *model) closePane() {
	t := m.tab()
	if len(t.panes) == 0 {
		return
	}
	t.panes[t.focus].detach()
	t.panes = append(t.panes[:t.focus], t.panes[t.focus+1:]...)
	if len(t.panes) == 0 {
		t.panes = []*pane{m.newPane()}
	}
	t.focus = min(t.focus, len(t.panes)-1)
	if m.mode == modeInsert {
		m.mode = modeNormal
	}
}

func (m *model) toggleSidebar() tea.Cmd {
	m.sidebarOpen = !m.sidebarOpen
	if m.animating {
		return nil
	}
	m.animating = true
	return m.frame()
}

// ---- update ----

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		return m, nil

	case frameMsg:
		target := 0.0
		if m.sidebarOpen {
			target = sidebarOpenWidth
		}
		m.sidebarX, m.sidebarV = m.spring.Update(m.sidebarX, m.sidebarV, target)
		if math.Abs(m.sidebarX-target) < 0.3 && math.Abs(m.sidebarV) < 0.3 {
			m.sidebarX, m.sidebarV, m.animating = target, 0, false
			return m, nil
		}
		return m, m.frame()

	case pollMsg:
		m.layout()
		var cmds []tea.Cmd
		for _, p := range m.tab().panes {
			if p.container == "" || p.inflight {
				continue
			}
			if r, ok := m.rects[p.id]; ok && p.out != nil {
				w, h := max(10, r.w-2), max(3, r.h-2)
				if p.sized != [2]int{w, h} {
					p.sized = [2]int{w, h}
					p.out.send(sendItem{raw: []string{"resize-window", "-t", persona.SessionName, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h)}})
				}
			}
			p.inflight = true
			cmds = append(cmds, capture(m.ctx, p.id, p.container))
		}
		cmds = append(cmds, m.poll())
		return m, tea.Batch(cmds...)

	case captureMsg:
		if p := m.findPane(msg.pane); p != nil {
			p.inflight = false
			if msg.err != nil {
				if m.containerRunning(p.container) {
					p.err = "waiting for Claude Code session…"
				} else {
					p.err = "container is not running"
				}
			} else {
				p.err, p.lines, p.cursorX, p.cursorY = "", msg.lines, msg.cx, msg.cy
			}
		}
		return m, nil

	case refreshMsg:
		return m, tea.Batch(m.load(), tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshMsg{} }))

	case dataMsg:
		if msg.err != nil {
			m.setStatus(true, "docker: %v", msg.err)
		}
		m.personas, m.containers = msg.personas, msg.containers
		m.rebuildItems()
		if !m.seeded {
			m.seeded = true
			n := 0
			for _, c := range m.containers {
				if c.Running() && n < 4 {
					m.openContainer(c.Name, c.Persona)
					n++
				}
			}
			if len(m.personas) == 0 {
				m.setStatus(false, "No personas yet — create one with `persona new`.")
			}
		}
		return m, nil

	case spawnedMsg:
		m.busy--
		p := m.findPane(msg.pane)
		if msg.err != nil {
			m.setStatus(true, "spawn %s failed: %v", msg.persona, msg.err)
			if p != nil {
				p.starting = false
				p.err = msg.err.Error()
			}
			return m, nil
		}
		if p != nil {
			p.attach(m.ctx, msg.container, msg.persona)
		}
		m.setStatus(false, "Spawned %s", msg.container)
		return m, m.load()

	case stoppedMsg:
		m.busy--
		if msg.err != nil {
			m.setStatus(true, "stop %s: %v", msg.container, msg.err)
		} else {
			m.setStatus(false, "Stopped %s", msg.container)
		}
		return m, m.load()

	case attachedMsg:
		for _, t := range m.tabs {
			for _, p := range t.panes {
				p.sized = [2]int{} // force a resize back to pane dimensions
			}
		}
		if msg.err != nil {
			m.setStatus(true, "attach: %v", msg.err)
		}
		return m, nil

	case spinner.TickMsg:
		if m.busy <= 0 {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.PasteMsg:
		if p := m.focused(); m.mode == modeInsert && p != nil && p.out != nil {
			p.out.send(sendItem{literal: true, keys: msg.Content})
		}
		return m, nil

	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		for i, p := range m.tab().panes {
			if r, ok := m.rects[p.id]; ok && r.contains(mouse.X, mouse.Y) {
				m.tab().focus = i
				return m, nil
			}
		}
		if mouse.X < m.sidebarWidth() && mouse.Y >= 3 {
			if idx := mouse.Y - 3; idx < len(m.items) {
				m.cursor = idx
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.mode == modeInsert {
			return m, m.handleInsert(msg)
		}
		return m, m.handleNormal(msg)
	}
	return m, nil
}

func (m *model) handleInsert(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, keys.Leave) {
		m.mode = modeNormal
		return nil
	}
	p := m.focused()
	if p == nil || p.out == nil {
		m.mode = modeNormal
		return nil
	}
	if name, literal, ok := tmuxKey(msg); ok {
		p.out.send(sendItem{literal: literal, keys: name})
	}
	return nil
}

func (m *model) handleNormal(msg tea.KeyPressMsg) tea.Cmd {
	if m.showHelp && !key.Matches(msg, keys.Quit) {
		m.showHelp = false
		return nil
	}
	t := m.tab()
	switch {
	case key.Matches(msg, keys.Quit):
		return tea.Quit
	case key.Matches(msg, keys.Help):
		m.showHelp = true
	case key.Matches(msg, keys.Up):
		m.cursor = max(0, m.cursor-1)
	case key.Matches(msg, keys.Down):
		m.cursor = min(len(m.items)-1, m.cursor+1)
	case key.Matches(msg, keys.Select):
		if m.cursor < len(m.items) {
			it := m.items[m.cursor]
			if it.container == "" {
				return m.spawnInto(it.persona, false)
			}
			if it.state != "running" {
				m.setStatus(true, "%s is %s", it.container, it.state)
				return nil
			}
			m.openContainer(it.container, it.persona)
		}
	case key.Matches(msg, keys.Spawn):
		if m.cursor < len(m.items) {
			return m.spawnInto(m.items[m.cursor].persona, true)
		}
	case key.Matches(msg, keys.NewPane):
		t.panes = append(t.panes, m.newPane())
		t.focus = len(t.panes) - 1
	case key.Matches(msg, keys.ClosePane):
		m.closePane()
	case key.Matches(msg, keys.StopPane):
		p := m.focused()
		if p == nil || p.container == "" {
			return nil
		}
		name, ctx := p.container, m.ctx
		m.closePane()
		m.busy++
		m.setStatus(false, "Stopping %s…", name)
		return tea.Batch(m.spin.Tick, func() tea.Msg {
			err := docker.Stop(ctx, name)
			if err == nil {
				err = docker.Remove(ctx, name)
			}
			return stoppedMsg{container: name, err: err}
		})
	case key.Matches(msg, keys.Insert):
		if p := m.focused(); p != nil && p.out != nil {
			m.mode = modeInsert
		} else {
			m.setStatus(true, "focused pane has no running session — select one in the sidebar and press enter")
		}
	case key.Matches(msg, keys.Attach):
		p := m.focused()
		if p == nil || p.container == "" {
			return nil
		}
		name, ctx := p.container, m.ctx
		_, _ = docker.Exec(ctx, name, "tmux", "set-option", "-t", persona.SessionName, "window-size", "latest")
		return tea.ExecProcess(attachCmd(ctx, name), func(err error) tea.Msg { return attachedMsg{err: err} })
	case key.Matches(msg, keys.NextPane):
		if len(t.panes) > 0 {
			t.focus = (t.focus + 1) % len(t.panes)
		}
	case key.Matches(msg, keys.PrevPane):
		if len(t.panes) > 0 {
			t.focus = (t.focus - 1 + len(t.panes)) % len(t.panes)
		}
	case key.Matches(msg, keys.NewTab):
		m.tabs = append(m.tabs, &tab{panes: []*pane{m.newPane()}})
		m.active = len(m.tabs) - 1
	case key.Matches(msg, keys.CloseTab):
		for _, p := range t.panes {
			p.detach()
		}
		m.tabs = append(m.tabs[:m.active], m.tabs[m.active+1:]...)
		if len(m.tabs) == 0 {
			m.tabs = []*tab{{panes: []*pane{m.newPane()}}}
		}
		m.active = min(m.active, len(m.tabs)-1)
	case key.Matches(msg, keys.NextTab):
		m.active = (m.active + 1) % len(m.tabs)
	case key.Matches(msg, keys.PrevTab):
		m.active = (m.active - 1 + len(m.tabs)) % len(m.tabs)
	case key.Matches(msg, keys.Sidebar):
		return m.toggleSidebar()
	case key.Matches(msg, keys.Rotate):
		t.vertical = !t.vertical
	case key.Matches(msg, keys.Refresh):
		return m.load()
	default:
		if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			if i := int(s[0] - '1'); i < len(m.tabs) {
				m.active = i
			}
		}
	}
	return nil
}

func attachCmd(ctx context.Context, container string) *exec.Cmd {
	c := docker.AttachCommand(ctx, container)
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil // let Bubble Tea wire the terminal
	return c
}

// ---- layout & view ----

// layout computes pane rectangles for the active tab.
func (m *model) layout() {
	m.rects = map[int]rect{}
	t := m.tab()
	n := len(t.panes)
	if n == 0 || m.width == 0 {
		return
	}
	x0, y0 := m.sidebarWidth(), 1
	w, h := m.width-x0, m.height-2
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := int(math.Ceil(float64(n) / float64(cols)))
	if t.vertical {
		cols, rows = rows, cols
	}
	i := 0
	for r := 0; r < rows && i < n; r++ {
		inRow := min(cols, n-i)
		y := y0 + r*h/rows
		ph := y0 + (r+1)*h/rows - y
		for c := 0; c < inRow; c++ {
			x := x0 + c*w/inRow
			pw := x0 + (c+1)*w/inRow - x
			m.rects[t.panes[i].id] = rect{x, y, pw, ph}
			i++
		}
	}
}

func (m *model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "persona · control center"
	if m.width == 0 {
		return v
	}
	m.layout()

	screen := make([]string, m.height)
	screen[0] = m.renderTabs()
	body := m.renderBody()
	for i := 0; i < m.height-2 && i < len(body); i++ {
		screen[i+1] = body[i]
	}
	screen[m.height-1] = m.renderFooter()
	v.Content = strings.Join(screen, "\n")

	if m.mode == modeInsert && !m.showHelp {
		if p := m.focused(); p != nil && p.err == "" && p.lines != nil {
			if r, ok := m.rects[p.id]; ok && p.cursorX < r.w-2 && p.cursorY < r.h-2 {
				v.Cursor = tea.NewCursor(r.x+1+p.cursorX, r.y+1+p.cursorY)
			}
		}
	}
	return v
}

var (
	tabActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1A1A24")).Background(ui.Accent).Padding(0, 1)
	tabInactive = lipgloss.NewStyle().Foreground(ui.Muted).Padding(0, 1)
	brand       = lipgloss.NewStyle().Bold(true).Foreground(ui.Accent).Padding(0, 1)
	badgeNormal = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1A1A24")).Background(ui.Accent).Padding(0, 1)
	badgeInsert = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1A1A24")).Background(ui.Accent2).Padding(0, 1)
	selRow      = lipgloss.NewStyle().Background(ui.Subtle).Bold(true)
)

func (m *model) renderTabs() string {
	parts := []string{brand.Render("✦ persona")}
	for i, t := range m.tabs {
		label := strconv.Itoa(i + 1)
		if len(t.panes) > 0 {
			if p := t.panes[t.focus]; p.persona != "" {
				label += " " + p.persona
			}
		}
		if len(t.panes) > 1 {
			label += fmt.Sprintf(" (%d)", len(t.panes))
		}
		if i == m.active {
			parts = append(parts, tabActive.Render(label))
		} else {
			parts = append(parts, tabInactive.Render(label))
		}
	}
	return fit(strings.Join(parts, " "), m.width)
}

func (m *model) renderFooter() string {
	badge := badgeNormal.Render("NORMAL")
	hint := m.help.ShortHelpView(keys.ShortHelp())
	if m.mode == modeInsert {
		badge = badgeInsert.Render("INSERT")
		hint = ui.Dim.Render("keys go to the pane · ctrl+] to leave")
	}
	status := ""
	if m.busy > 0 {
		status = m.spin.View() + " "
	}
	if m.status != "" {
		if m.isError {
			status += ui.ErrS.Render(m.status)
		} else {
			status += m.status
		}
	}
	left := badge + " " + status
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(hint)
	if gap < 1 && m.mode == modeNormal {
		hint = m.help.ShortHelpView([]key.Binding{keys.Insert, keys.Help, keys.Quit})
		gap = m.width - ansi.StringWidth(left) - ansi.StringWidth(hint)
	}
	if gap < 1 {
		return fit(left, m.width)
	}
	return left + strings.Repeat(" ", gap) + hint
}

func (m *model) renderBody() []string {
	h := m.height - 2
	lines := make([]string, h)
	sw := m.sidebarWidth()
	side := m.renderSidebar(sw, h)

	panesW := m.width - sw
	var area []string
	if m.showHelp {
		helpBox := ui.Box.Render(ui.Title.Render("Control center") + "\n\n" + m.help.FullHelpView(keys.FullHelp()) +
			"\n\n" + ui.Dim.Render("Normal mode navigates; INSERT mode (i) types into the focused pane.\nPress any key to close."))
		placed := lipgloss.Place(panesW, h, lipgloss.Center, lipgloss.Center, helpBox)
		area = strings.Split(placed, "\n")
	} else {
		area = m.renderPanes(panesW, h)
	}
	for i := 0; i < h; i++ {
		l := ""
		if i < len(side) {
			l = side[i]
		}
		a := ""
		if i < len(area) {
			a = area[i]
		}
		lines[i] = l + fit(a, panesW)
	}
	return lines
}

func (m *model) renderSidebar(w, h int) []string {
	out := make([]string, h)
	if w < 4 {
		for i := range out {
			out[i] = strings.Repeat(" ", w)
		}
		return out
	}
	inner := w - 2
	add := func(i int, s string) {
		if i < h {
			out[i] = " " + fit(s, inner) + " "
		}
	}
	add(0, ui.Heading.Render("PERSONAS"))
	add(1, ui.Dim.Render(strings.Repeat("─", inner)))
	const first = 2 // body row of the first item (screen row 3)
	if len(m.items) == 0 {
		add(first, ui.Dim.Render("none — run `persona new`"))
	}
	for i, it := range m.items {
		var s string
		if it.container == "" {
			s = ui.Bold.Render("◆ " + it.persona)
		} else {
			dot := ui.OK.Render("●")
			if it.state != "running" {
				dot = ui.Dim.Render("○")
			}
			short := strings.TrimPrefix(it.container, "persona-"+it.persona+"-")
			s = "  " + dot + " " + short
		}
		s = fit(s, inner)
		if i == m.cursor && m.mode == modeNormal {
			s = selRow.Render(ansi.Strip(s))
		}
		add(first+i, s)
	}
	for i := range out {
		if out[i] == "" {
			out[i] = strings.Repeat(" ", w)
		}
		out[i] = fit(out[i], w-1) + ui.Dim.Render("│")
	}
	return out
}

func (m *model) renderPanes(w, h int) []string {
	area := make([]string, h)
	for i := range area {
		area[i] = strings.Repeat(" ", w)
	}
	t := m.tab()
	// Group panes by row (same y), compose each screen line left to right.
	type cell struct {
		r     rect
		lines []string
	}
	rowsByY := map[int][]cell{}
	for i, p := range t.panes {
		r, ok := m.rects[p.id]
		if !ok {
			continue
		}
		rowsByY[r.y] = append(rowsByY[r.y], cell{r, m.renderPane(p, r, i == t.focus)})
	}
	for _, cells := range rowsByY {
		for line := 0; line < cells[0].r.h; line++ {
			var b strings.Builder
			for _, c := range cells {
				if line < len(c.lines) {
					b.WriteString(c.lines[line])
				}
			}
			y := cells[0].r.y - 1 + line
			if y >= 0 && y < h {
				area[y] = b.String()
			}
		}
	}
	return area
}

func (m *model) renderPane(p *pane, r rect, focused bool) []string {
	border := lipgloss.NewStyle().Foreground(ui.Subtle)
	if focused {
		border = lipgloss.NewStyle().Foreground(ui.Accent)
		if m.mode == modeInsert {
			border = lipgloss.NewStyle().Foreground(ui.Accent2)
		}
	}
	iw, ih := max(0, r.w-2), max(0, r.h-2)

	title := " empty "
	switch {
	case p.starting:
		title = " " + p.persona + " · starting… "
	case p.container != "":
		title = " " + strings.TrimPrefix(p.container, "persona-") + " "
	}
	title = ansi.Truncate(title, max(0, iw-2), "…")
	top := border.Render("╭─") + lipgloss.NewStyle().Bold(focused).Render(title) +
		border.Render(strings.Repeat("─", max(0, iw-1-ansi.StringWidth(title)))+"╮")

	var body []string
	switch {
	case p.container == "" && !p.starting:
		msg := ui.Dim.Render("select a persona or instance in the sidebar and press enter")
		if p.err != "" {
			msg = ui.ErrS.Render(p.err)
		}
		body = strings.Split(lipgloss.Place(iw, ih, lipgloss.Center, lipgloss.Center, lipgloss.NewStyle().Width(max(1, iw-4)).Align(lipgloss.Center).Render(msg)), "\n")
	case p.starting || (p.err != "" && p.lines == nil):
		msg := p.err
		if p.starting || msg == "" {
			msg = "starting container…"
		}
		body = strings.Split(lipgloss.Place(iw, ih, lipgloss.Center, lipgloss.Center, ui.Dim.Render(msg)), "\n")
	default:
		body = p.lines
	}

	out := make([]string, 0, r.h)
	out = append(out, top)
	side := border.Render("│")
	for i := 0; i < ih; i++ {
		l := ""
		if i < len(body) {
			l = body[i]
		}
		out = append(out, side+fit(l, iw)+"\x1b[0m"+side)
	}
	bottom := "╰" + strings.Repeat("─", iw) + "╯"
	if p.err != "" && p.lines != nil {
		note := " " + p.err + " "
		if ansi.StringWidth(note) < iw-2 {
			bottom = "╰─" + note + strings.Repeat("─", iw-1-ansi.StringWidth(note)) + "╯"
		}
	}
	out = append(out, border.Render(bottom))
	return out
}

// fit truncates or pads an ANSI string to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += "\x1b[0m" + strings.Repeat(" ", pad)
	}
	return s
}
