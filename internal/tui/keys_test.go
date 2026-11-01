package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestTmuxKey(t *testing.T) {
	cases := []struct {
		k       tea.Key
		want    string
		literal bool
	}{
		{tea.Key{Code: 'a', Text: "a"}, "a", true},
		{tea.Key{Code: tea.KeyEnter}, "Enter", false},
		{tea.Key{Code: 'c', Mod: tea.ModCtrl}, "C-c", false},
		{tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}, "BTab", false},
		{tea.Key{Code: tea.KeyUp, Mod: tea.ModAlt}, "M-Up", false},
		{tea.Key{Code: tea.KeyEscape}, "Escape", false},
	}
	for _, c := range cases {
		got, lit, ok := tmuxKey(tea.KeyPressMsg(c.k))
		if !ok || got != c.want || lit != c.literal {
			t.Errorf("tmuxKey(%v) = %q,%v,%v want %q,%v", c.k, got, lit, ok, c.want, c.literal)
		}
	}
}
