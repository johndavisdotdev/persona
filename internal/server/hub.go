package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// hub polls the state and hands it to every control stream when it changes
// (and to each new one at once).
type hub struct {
	mu      sync.Mutex
	subs    map[chan State]struct{}
	last    *State
	lastRaw []byte
	pokec   chan struct{}
}

const pollEvery = 2 * time.Second

func newHub(ctx context.Context, a Actions) *hub {
	h := &hub{subs: map[chan State]struct{}{}, pokec: make(chan struct{}, 1)}
	go func() {
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for {
			h.refresh(ctx, a)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-h.pokec:
			}
		}
	}()
	return h
}

func (h *hub) poke() {
	select {
	case h.pokec <- struct{}{}:
	default:
	}
}

func (h *hub) refresh(ctx context.Context, a Actions) {
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n == 0 && h.last != nil {
		return // nobody watching: don't poll the runtime
	}
	s := a.State(ctx)
	raw, _ := json.Marshal(s)
	h.mu.Lock()
	defer h.mu.Unlock()
	if string(raw) == string(h.lastRaw) {
		return
	}
	h.last, h.lastRaw = &s, raw
	for c := range h.subs {
		select {
		case c <- s:
		default: // a slow page gets the next one
		}
	}
}

// subscribe returns states until ctx ends, starting with the latest.
func (h *hub) subscribe(ctx context.Context) <-chan State {
	c := make(chan State, 1)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	if h.last != nil {
		c <- *h.last
	}
	h.mu.Unlock()
	h.poke()
	out := make(chan State)
	go func() {
		defer close(out)
		defer func() {
			h.mu.Lock()
			delete(h.subs, c)
			h.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-c:
				select {
				case out <- s:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// noticeHub hands every notice to every control stream subscribed when it
// arrives (notices aren't state: a page that connects later doesn't get
// the old ones).
type noticeHub struct {
	mu   sync.Mutex
	subs map[chan Notice]struct{}
}

func newNoticeHub(ctx context.Context, in <-chan Notice) *noticeHub {
	h := &noticeHub{subs: map[chan Notice]struct{}{}}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case n, ok := <-in:
				if !ok {
					return
				}
				h.mu.Lock()
				for c := range h.subs {
					select {
					case c <- n:
					default: // a stuck page misses it rather than blocking the rest
					}
				}
				h.mu.Unlock()
			}
		}
	}()
	return h
}

func (h *noticeHub) subscribe(ctx context.Context) <-chan Notice {
	c := make(chan Notice, 32)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	out := make(chan Notice)
	go func() {
		defer close(out)
		defer func() {
			h.mu.Lock()
			delete(h.subs, c)
			h.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-c:
				select {
				case out <- n:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
