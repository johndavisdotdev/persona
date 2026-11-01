package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/johndavisdev/persona/internal/persona"
)

type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// pane mirrors one container's Claude Code tmux session.
type pane struct {
	id        int
	container string
	persona   string
	starting  bool
	lines     []string
	cursorX   int
	cursorY   int
	err       string
	inflight  bool
	sized     [2]int // last tmux window size applied
	out       *sender
}

func (p *pane) attach(ctx context.Context, container, personaName string) {
	p.detach()
	p.container, p.persona, p.starting = container, personaName, false
	p.lines, p.err, p.sized = nil, "", [2]int{}
	p.out = newSender(ctx, container)
}

func (p *pane) detach() {
	if p.out != nil {
		p.out.close()
		p.out = nil
	}
	p.container, p.persona, p.lines, p.err = "", "", nil, ""
}

type captureMsg struct {
	pane   int
	lines  []string
	cx, cy int
	err    error
}

const sep = "\x1e"

// capture fetches the cursor position and the visible screen with ANSI styles.
func capture(ctx context.Context, id int, container string) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		script := `tmux display -p -t ` + persona.SessionName + ` '#{cursor_x} #{cursor_y}' && printf '` + sep + `' && tmux capture-pane -p -e -N -t ` + persona.SessionName
		out, err := rt.Exec(cctx, container, "sh", "-c", script)
		if err != nil {
			return captureMsg{pane: id, err: err}
		}
		head, body, _ := strings.Cut(string(out), sep)
		var cx, cy int
		if f := strings.Fields(head); len(f) == 2 {
			cx, _ = strconv.Atoi(f[0])
			cy, _ = strconv.Atoi(f[1])
		}
		body = strings.TrimSuffix(body, "\n")
		return captureMsg{pane: id, lines: strings.Split(body, "\n"), cx: cx, cy: cy}
	}
}

// sendItem is one queued tmux operation.
type sendItem struct {
	literal bool
	keys    string
	raw     []string
}

// sender serializes tmux commands per container so keystrokes keep their order.
type sender struct {
	ch     chan sendItem
	cancel context.CancelFunc
}

func newSender(parent context.Context, container string) *sender {
	ctx, cancel := context.WithCancel(parent)
	s := &sender{ch: make(chan sendItem, 512), cancel: cancel}
	go func() {
		for {
			var it sendItem
			select {
			case <-ctx.Done():
				return
			case it = <-s.ch:
			}
			// Coalesce consecutive literal text into one exec.
			if it.literal {
			drain:
				for {
					select {
					case next := <-s.ch:
						if next.literal {
							it.keys += next.keys
							continue
						}
						s.run(ctx, container, it)
						it = next
						if !it.literal {
							break drain
						}
					default:
						break drain
					}
				}
			}
			s.run(ctx, container, it)
		}
	}()
	return s
}

func (s *sender) run(ctx context.Context, container string, it sendItem) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"tmux"}
	switch {
	case it.raw != nil:
		args = append(args, it.raw...)
	case it.literal:
		args = append(args, "send-keys", "-t", persona.SessionName, "-l", "--", it.keys)
	default:
		args = append(args, "send-keys", "-t", persona.SessionName, it.keys)
	}
	_, _ = rt.Exec(cctx, container, args...)
}

func (s *sender) send(it sendItem) {
	select {
	case s.ch <- it:
	default: // drop if the container is unresponsive rather than blocking the UI
	}
}

func (s *sender) close() { s.cancel() }
