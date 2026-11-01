//go:build linux

package instance

import (
	"encoding/json"
	"testing"

	"github.com/johndavisdev/persona/internal/status"
)

func TestBeaconStatus(t *testing.T) {
	for _, tc := range []struct {
		body    string
		event   string
		options int
	}{
		{`{"finished":{}}`, status.EventTaskDone, 0},
		{`{"needInput":{"question":"DB?","closedSelection":{"choices":["pg","sqlite"]}}}`, status.EventQuestion, 2},
		{`{"needInput":{"question":"Ship?","confirmation":{}}}`, status.EventQuestion, 2},
		{`{"command":{"command":"stop"}}`, status.EventNotification, 0},
	} {
		raw, err := beaconStatus(json.RawMessage(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		var ev status.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Event != tc.event {
			t.Errorf("%s: event %q, want %q", tc.body, ev.Event, tc.event)
		}
		if tc.event == status.EventQuestion && len(ev.Questions[0].Options) != tc.options {
			t.Errorf("%s: %d options, want %d", tc.body, len(ev.Questions[0].Options), tc.options)
		}
	}
	// A question puts the spawn in "needs input" for persona manage.
	raw, _ := beaconStatus(json.RawMessage(`{"needInput":{"question":"DB?"}}`))
	var ev status.Event
	_ = json.Unmarshal(raw, &ev)
	if st, _ := status.Derive([]status.Event{ev}); st != status.NeedsInput {
		t.Errorf("state %q, want %q", st, status.NeedsInput)
	}
	for _, bad := range []string{`{}`, `{"needInput":{}}`, `not json`} {
		if _, err := beaconStatus(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
