//go:build linux

package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johndavisdev/persona/internal/status"
)

// persona-beacon is an MCP server (beacon/, .NET) that every image gets
// through the beacon plugin of the local "persona" marketplace (see package
// persona). It gives the agent two channels:
//
//   - other agents, by UDP multicast on the bridge every VM shares
//     (discovery, broadcast commands), and
//   - this host's broker, through persona-agent: guest request kind
//     agent.KindBeacon, whose body is a BrokerMessage (beacon/Protos/shapes.proto)
//     in protobuf's JSON mapping.
//
// brokerMessage is BrokerMessage in protobuf's JSON mapping; exactly one
// field is set.
type brokerMessage struct {
	Finished *struct{} `json:"finished"`
	Command  *struct {
		Command string `json:"command"`
	} `json:"command"`
	NeedInput *struct {
		Question     string `json:"question"`
		Confirmation *struct {
			YesText string `json:"yesText"`
			NoText  string `json:"noText"`
		} `json:"confirmation"`
		ClosedSelection *selection `json:"closedSelection"`
		OpenSelection   *selection `json:"openSelection"`
	} `json:"needInput"`
}

type selection struct {
	Choices []string `json:"choices"`
}

// beaconStatus turns a BrokerMessage into the status event persona manage
// shows for it.
func beaconStatus(body json.RawMessage) ([]byte, error) {
	var m brokerMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("beacon: bad message: %w", err)
	}
	ev := status.Event{Harness: "beacon", TS: time.Now().UTC()}
	switch {
	case m.Finished != nil:
		ev.Event, ev.Task = status.EventTaskDone, "work finished"
	case m.NeedInput != nil:
		n := m.NeedInput
		if strings.TrimSpace(n.Question) == "" {
			return nil, errors.New("beacon: need_input has no question")
		}
		q := status.Question{Question: n.Question, Header: "Input needed"}
		switch {
		case n.Confirmation != nil:
			q.Options = []status.Option{{Label: or(n.Confirmation.YesText, "Yes")}, {Label: or(n.Confirmation.NoText, "No")}}
		case n.ClosedSelection != nil:
			q.Options = options(n.ClosedSelection.Choices)
		case n.OpenSelection != nil:
			q.Options = options(n.OpenSelection.Choices)
		}
		ev.Event, ev.Questions = status.EventQuestion, []status.Question{q}
	case m.Command != nil:
		ev.Event, ev.Message = status.EventNotification, "command: "+m.Command.Command
	default:
		return nil, errors.New("beacon: empty message")
	}
	return json.Marshal(ev)
}

func options(choices []string) []status.Option {
	out := make([]status.Option, 0, len(choices))
	for _, c := range choices {
		out = append(out, status.Option{Label: c})
	}
	return out
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
