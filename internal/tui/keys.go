package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

type keyMap struct {
	Up, Down, Select, Spawn, NewPane, ClosePane, StopPane     key.Binding
	Insert, Leave, Attach, NextPane, PrevPane                 key.Binding
	NewTab, CloseTab, NextTab, PrevTab, Sidebar, Rotate, Help key.Binding
	Refresh, Quit                                             key.Binding
}

var keys = keyMap{
	Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Select:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open/spawn into pane")),
	Spawn:     key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "spawn persona in new pane")),
	NewPane:   key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "split: new pane")),
	ClosePane: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "close pane")),
	StopPane:  key.NewBinding(key.WithKeys("X"), key.WithHelp("X", "kill the pane's spawn")),
	Insert:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "type into pane")),
	Leave:     key.NewBinding(key.WithKeys("ctrl+]", "f12"), key.WithHelp("ctrl+]", "back to normal")),
	Attach:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "full-screen attach")),
	NextPane:  key.NewBinding(key.WithKeys("tab", "l"), key.WithHelp("tab", "next pane")),
	PrevPane:  key.NewBinding(key.WithKeys("shift+tab", "h"), key.WithHelp("shift+tab", "prev pane")),
	NewTab:    key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "new tab")),
	CloseTab:  key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "close tab")),
	NextTab:   key.NewBinding(key.WithKeys("]"), key.WithHelp("]/1-9", "next tab")),
	PrevTab:   key.NewBinding(key.WithKeys("["), key.WithHelp("[", "prev tab")),
	Sidebar:   key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "toggle sidebar")),
	Rotate:    key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "rotate layout")),
	Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Refresh:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Select, k.Insert, k.Leave, k.Spawn, k.NextPane, k.NewTab, k.Attach, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Select, k.Spawn, k.Refresh},
		{k.Insert, k.Leave, k.Attach, k.StopPane},
		{k.NewPane, k.ClosePane, k.NextPane, k.PrevPane, k.Rotate},
		{k.NewTab, k.CloseTab, k.NextTab, k.PrevTab, k.Sidebar, k.Quit},
	}
}

// tmuxKey converts a key press into tmux send-keys arguments. literal reports
// whether the result should be sent with -l (as text) rather than as a key name.
func tmuxKey(msg tea.KeyPressMsg) (keyName string, literal bool, ok bool) {
	k := msg.Key()
	ctrl, alt, shift := k.Mod&tea.ModCtrl != 0, k.Mod&tea.ModAlt != 0, k.Mod&tea.ModShift != 0
	if k.Text != "" && !ctrl && !alt {
		return k.Text, true, true
	}
	named := map[rune]string{
		tea.KeyEnter: "Enter", tea.KeyTab: "Tab", tea.KeyBackspace: "BSpace", tea.KeyEscape: "Escape",
		tea.KeyUp: "Up", tea.KeyDown: "Down", tea.KeyLeft: "Left", tea.KeyRight: "Right",
		tea.KeyHome: "Home", tea.KeyEnd: "End", tea.KeyPgUp: "PPage", tea.KeyPgDown: "NPage",
		tea.KeyDelete: "DC", tea.KeyInsert: "IC", tea.KeySpace: "Space",
		tea.KeyF1: "F1", tea.KeyF2: "F2", tea.KeyF3: "F3", tea.KeyF4: "F4", tea.KeyF5: "F5", tea.KeyF6: "F6",
		tea.KeyF7: "F7", tea.KeyF8: "F8", tea.KeyF9: "F9", tea.KeyF10: "F10", tea.KeyF11: "F11",
	}
	var b strings.Builder
	if ctrl {
		b.WriteString("C-")
	}
	if alt {
		b.WriteString("M-")
	}
	if name, isNamed := named[k.Code]; isNamed {
		if k.Code == tea.KeyTab && shift && !ctrl && !alt {
			return "BTab", false, true
		}
		if shift {
			b.WriteString("S-")
		}
		b.WriteString(name)
		return b.String(), false, true
	}
	if k.Code > 0x20 && k.Code < 0x10FFFF {
		b.WriteRune(k.Code)
		return b.String(), false, true
	}
	return "", false, false
}
