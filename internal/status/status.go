// Package status stores and interprets the status stream spawns produce: one
// event per harness hook (prompt submitted, turn finished, notification,
// question for the user, task completed), as JSON lines in
// $PERSONA_HOME/status/<spawn>/events.jsonl.
//
// MicroVM spawns report through persona-agent and the broker, which appends
// here; Docker spawns get the directory bind-mounted at MountPath and append
// to it directly. persona manage reads it.
package status

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johndavisdev/persona/internal/config"
)

// MountPath is where a Docker spawn sees its status directory.
const MountPath = "/run/persona-status"

// FileName is the events file inside a status directory.
const FileName = "events.jsonl"

// Hook event names, as persona-status reports them (Claude Code's names;
// other harnesses map their lifecycle onto these).
const (
	EventPrompt       = "UserPromptSubmit"
	EventStop         = "Stop"
	EventStopFailure  = "StopFailure" // the turn ended in an API error
	EventNotification = "Notification"
	EventTaskDone     = "TaskCompleted"
	EventQuestion     = "Question" // AskUserQuestion (or the harness's equivalent)
	EventSessionStart = "SessionStart"
)

// Event is one status report.
type Event struct {
	Event            string     `json:"event"`
	Harness          string     `json:"harness,omitempty"`
	TS               time.Time  `json:"ts"`
	NotificationType string     `json:"notification_type,omitempty"`
	Title            string     `json:"title,omitempty"`
	Message          string     `json:"message,omitempty"`
	LastMessage      string     `json:"last_message,omitempty"`
	Questions        []Question `json:"questions,omitempty"`
	Task             string     `json:"task,omitempty"`
	Error            string     `json:"error,omitempty"`
	Prompt           string     `json:"prompt,omitempty"`
}

// Question is a question the agent put to the user, with its choices.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

// Option is one answer to a Question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Dir is a spawn's status directory.
func Dir(spawn string) string { return filepath.Join(config.Home(), "status", spawn) }

// Prepare creates a spawn's status directory. Docker spawns write to it as
// the image's user (uid 1000), so it is world-writable unless that is us.
func Prepare(spawn string) (string, error) {
	d := Dir(spawn)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	mode := os.FileMode(0o755)
	if os.Getuid() != 1000 {
		mode = 0o777
	}
	return d, os.Chmod(d, mode)
}

// Append records one event (raw JSON from a spawn) for spawn.
func Append(spawn string, raw []byte) error {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil || ev.Event == "" {
		return nil // not ours; ignore rather than fail the spawn's hook
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	d, err := Prepare(spawn)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(ev)
	f, err := os.OpenFile(filepath.Join(d, FileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns the events after byte offset off, and the new offset.
func Read(spawn string, off int64) ([]Event, int64, error) {
	f, err := os.Open(filepath.Join(Dir(spawn), FileName))
	if os.IsNotExist(err) {
		return nil, 0, nil
	} else if err != nil {
		return nil, off, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < off {
		off = 0 // truncated or replaced
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err
	}
	var out []Event
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // a partial last line is read next time
			break
		}
		off += int64(len(line))
		var ev Event
		if json.Unmarshal(line, &ev) == nil && ev.Event != "" {
			out = append(out, ev)
		}
	}
	return out, off, nil
}

// Remove deletes a spawn's status history.
func Remove(spawn string) error { return os.RemoveAll(Dir(spawn)) }

// State is what a spawn is doing, as far as its status stream says.
type State string

const (
	Unknown    State = "unknown"
	Working    State = "working"
	Idle       State = "idle"        // finished its turn; ready for the next prompt
	NeedsInput State = "needs input" // permission prompt, MCP elicitation, or a question
	Failed     State = "failed"      // the last turn ended in an error
)

// needsInput are notification types that mean the agent is waiting on you.
var needsInput = map[string]bool{
	"permission_prompt": true, "elicitation_dialog": true, "elicitation_url_dialog": true, "agent_needs_input": true,
}

// Derive replays events into the current state, and the latest question or
// input request (nil once the agent has moved on).
func Derive(events []Event) (State, *Event) {
	st, pending := Unknown, (*Event)(nil)
	for i := range events {
		ev := &events[i]
		switch ev.Event {
		case EventSessionStart, EventPrompt:
			st, pending = Working, nil
		case EventStop:
			st, pending = Idle, nil
		case EventStopFailure:
			st, pending = Failed, nil
		case EventQuestion:
			st, pending = NeedsInput, ev
		case EventNotification:
			switch {
			case needsInput[ev.NotificationType]:
				st, pending = NeedsInput, ev
			case ev.NotificationType == "idle_prompt":
				st, pending = Idle, nil
			}
		}
	}
	return st, pending
}

// Summary is a one-line description of an event.
func (e Event) Summary() string {
	clean := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	switch e.Event {
	case EventPrompt:
		return "▶ " + clean(e.Prompt)
	case EventStop:
		return "■ " + clean(e.LastMessage)
	case EventStopFailure:
		msg := e.Error
		if msg == "" {
			msg = e.LastMessage // Claude Code reports the API error here
		}
		if msg == "" {
			return "✗ the turn failed"
		}
		return "✗ " + clean(msg)
	case EventTaskDone:
		return "✓ task: " + clean(e.Task)
	case EventQuestion:
		if len(e.Questions) > 0 {
			return "? " + clean(e.Questions[0].Question)
		}
		return "? (question)"
	case EventNotification:
		return "! " + clean(e.Message)
	case EventSessionStart:
		return "● session started"
	}
	return clean(e.Event + " " + e.Message)
}
