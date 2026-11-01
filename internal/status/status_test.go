package status

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johndavisdev/persona/internal/config"
)

func TestDerive(t *testing.T) {
	ev := func(e, nt string) Event { return Event{Event: e, NotificationType: nt} }
	q := Event{Event: EventQuestion, Questions: []Question{{Question: "Which?"}}}
	cases := []struct {
		evs     []Event
		want    State
		pending bool
	}{
		{nil, Unknown, false},
		{[]Event{ev(EventSessionStart, ""), ev(EventPrompt, "")}, Working, false},
		{[]Event{ev(EventPrompt, ""), ev(EventStop, "")}, Idle, false},
		{[]Event{ev(EventPrompt, ""), q}, NeedsInput, true},
		{[]Event{ev(EventPrompt, ""), q, ev(EventPrompt, "")}, Working, false},
		{[]Event{ev(EventPrompt, ""), ev(EventNotification, "permission_prompt")}, NeedsInput, true},
		{[]Event{ev(EventStop, ""), ev(EventNotification, "idle_prompt")}, Idle, false},
		{[]Event{ev(EventPrompt, ""), ev(EventNotification, "auth_success")}, Working, false},
		{[]Event{ev(EventPrompt, ""), ev(EventStopFailure, "")}, Failed, false},
		{[]Event{ev(EventPrompt, ""), ev(EventTaskDone, "")}, Working, false},
	}
	for i, c := range cases {
		st, p := Derive(c.evs)
		if st != c.want || (p != nil) != c.pending {
			t.Errorf("case %d: %s pending=%v, want %s pending=%v", i, st, p != nil, c.want, c.pending)
		}
	}
}

func TestAppendRead(t *testing.T) {
	if err := config.Init("", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := Append("s1", []byte(`{"event":"Stop","last_message":"done"}`)); err != nil {
		t.Fatal(err)
	}
	Append("s1", []byte(`not json`))
	Append("s1", []byte(`{"no_event":true}`))
	evs, off, err := Read("s1", 0)
	if err != nil || len(evs) != 1 || evs[0].LastMessage != "done" || evs[0].TS.IsZero() {
		t.Fatalf("Read = %+v %d %v", evs, off, err)
	}
	// A partial line is left for the next read.
	f, _ := os.OpenFile(filepath.Join(Dir("s1"), FileName), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"event":"UserPromptSubmit","ts":"` + time.Now().UTC().Format(time.RFC3339) + `"`)
	f.Close()
	if evs, off2, _ := Read("s1", off); len(evs) != 0 || off2 != off {
		t.Errorf("partial line consumed: %v %d", evs, off2)
	}
	if err := Remove("s1"); err != nil {
		t.Fatal(err)
	}
}
