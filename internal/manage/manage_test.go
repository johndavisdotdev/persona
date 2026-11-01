package manage

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/johndavisdev/persona/internal/instance"
	"github.com/johndavisdev/persona/internal/status"
)

func TestMessagesNameSpawnAndPersona(t *testing.T) {
	now := time.Now()
	evs := []status.Event{
		{Event: status.EventPrompt, TS: now, Prompt: "set up persistence"},
		{Event: status.EventQuestion, TS: now, Questions: []status.Question{{Question: "Which database?",
			Options: []status.Option{{Label: "Postgres"}, {Label: "SQLite"}}}}},
	}
	r := &spawnRow{c: instance.Container{Instance: "keen-yak-89", Persona: "dotnet-dev", State: "running"}, events: evs}
	r.state, r.pending = status.Derive(evs)
	m := &model{rows: []*spawnRow{r}, width: 120, height: 30}
	screen := ansi.Strip(m.View().Content)
	for _, want := range []string{
		"keen-yak-89 (dotnet-dev): ▶ set up persistence",
		"keen-yak-89 (dotnet-dev): ? Which database?",
		"? keen-yak-89 (dotnet-dev): Which database?", // the pending question
		"1. Postgres",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
}
