// Package agent is the host side of persona-agent, the init process inside
// every persona microVM (cmd/persona-agent), plus the protocol both share.
//
// The host always dials: it connects to the VM's Firecracker vsock socket,
// asks for guest port Port, and sends one JSON request line. Guest-initiated
// requests (such as "please push my branch") travel back over a connection
// the host opened with Subscribe, so nothing on the host listens for the guest.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Port is the guest vsock port persona-agent listens on.
const Port = 1024

// GuestSocket is where guest programs reach the agent (`persona-agent request`).
const GuestSocket = "/run/persona/agent.sock"

// Request is the first line a connection carries.
type Request struct {
	Op       string    `json:"op"` // ping, identify, quiesce, exec, stream, write, winsize, subscribe
	Identity *Identity `json:"identity,omitempty"`
	Exec     *Exec     `json:"exec,omitempty"`
	Write    *Write    `json:"write,omitempty"`
	Winsize  *Winsize  `json:"winsize,omitempty"`
}

// Winsize resizes the terminal of a pty stream.
type Winsize struct {
	Session int    `json:"session"`
	Rows    uint16 `json:"rows"`
	Cols    uint16 `json:"cols"`
}

// Identity is what makes one clone of a snapshot distinct from its siblings.
type Identity struct {
	Addr         string   `json:"addr"` // CIDR, e.g. 10.200.0.12/16
	Gateway      string   `json:"gateway"`
	MAC          string   `json:"mac"`
	Hostname     string   `json:"hostname"`
	Nameservers  []string `json:"nameservers"`
	TimeUnixNano int64    `json:"time_unix_nano"`
	Seed         []byte   `json:"seed"` // mixed into the guest RNG
}

// Exec runs a program in the guest.
type Exec struct {
	Argv   []string `json:"argv"`
	Env    []string `json:"env,omitempty"` // added to a minimal default environment
	Dir    string   `json:"dir,omitempty"`
	User   string   `json:"user,omitempty"` // default root
	Stdin  []byte   `json:"stdin,omitempty"`
	Detach bool     `json:"detach,omitempty"` // start in a new session and return at once
	// Tty runs a stream under a pseudo-terminal of Rows x Cols, for
	// interactive sessions such as `tmux attach`.
	Tty  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Write creates or replaces a file in the guest atomically.
type Write struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	User string `json:"user,omitempty"` // owner, default root
	Data []byte `json:"data"`
}

// Response answers every request except stream, whose connection becomes the
// program's stdin/stdout after an OK response.
type Response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Exit    int    `json:"exit"`
	Pid     int    `json:"pid,omitempty"`
	Session int    `json:"session,omitempty"` // pty streams: the id Resize takes
	Stdout  []byte `json:"stdout,omitempty"`
	Stderr  []byte `json:"stderr,omitempty"`
}

// Event is a guest-initiated request forwarded to the host subscriber.
type Event struct {
	ID   uint64          `json:"id"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Guest-initiated request kinds the host broker understands.
const (
	KindPush = "push" // body: null
	KindPR   = "pr"   // body: PRRequest
)

// PRRequest asks the host to push the instance's branch and open a pull request.
type PRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// PushResult is the broker's answer to push and pr.
type PushResult struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	Remote string `json:"remote"`
	PR     string `json:"pr,omitempty"` // URL
}

// Reply answers an Event.
type Reply struct {
	ID    uint64          `json:"id"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// ReadLine reads one '\n'-terminated line a byte at a time, so nothing past it
// is consumed: after a stream request the rest of the connection is raw data.
func ReadLine(r io.Reader) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < 1<<20 {
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return line, nil
		}
		line = append(line, b[0])
	}
	return nil, errors.New("line too long")
}

// Client talks to one VM's agent through Firecracker's vsock socket.
type Client struct{ Vsock string }

// Dial opens a connection to the agent and sends req.
func (c Client) Dial(ctx context.Context, req Request) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Vsock)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", Port); err != nil {
		conn.Close()
		return nil, err
	}
	ack, err := ReadLine(conn)
	if err != nil || !strings.HasPrefix(string(ack), "OK ") {
		conn.Close()
		return nil, fmt.Errorf("agent: vsock connect: %q %v", ack, err)
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// Do sends a request and returns its response.
func (c Client) Do(ctx context.Context, req Request) (Response, error) {
	conn, err := c.Dial(ctx, req)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	line, err := ReadLine(conn)
	if err != nil {
		return Response{}, fmt.Errorf("agent %s: %w", req.Op, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("agent %s: %s", req.Op, resp.Error)
	}
	return resp, nil
}

// Wait polls until the agent answers, e.g. right after boot or restore.
func (c Client) Wait(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := c.Do(pctx, Request{Op: "ping"})
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("agent never answered: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Identify gives a booted or freshly restored VM its network identity,
// hostname, wall-clock time and fresh entropy.
func (c Client) Identify(ctx context.Context, id Identity) error {
	_, err := c.Do(ctx, Request{Op: "identify", Identity: &id})
	return err
}

// Quiesce prepares the guest for a snapshot: flushes disks and takes eth0
// down, so clones never send traffic with the template's address.
func (c Client) Quiesce(ctx context.Context) error {
	_, err := c.Do(ctx, Request{Op: "quiesce"})
	return err
}

// Run executes a program and collects its output.
func (c Client) Run(ctx context.Context, e Exec) (Response, error) {
	resp, err := c.Do(ctx, Request{Op: "exec", Exec: &e})
	if err == nil && resp.Exit != 0 {
		err = fmt.Errorf("agent: %s exited %d: %s", strings.Join(e.Argv, " "), resp.Exit, strings.TrimSpace(string(resp.Stderr)))
	}
	return resp, err
}

// WriteFile creates or replaces a file in the guest.
func (c Client) WriteFile(ctx context.Context, w Write) error {
	_, err := c.Do(ctx, Request{Op: "write", Write: &w})
	return err
}

// Stream starts a program whose stdin and stdout are the returned connection
// (stderr goes to the guest console). Used as an rsync/git transport.
func (c Client) Stream(ctx context.Context, e Exec) (net.Conn, error) {
	e.Tty = false
	conn, _, err := c.stream(ctx, e)
	return conn, err
}

// Pty starts a program on a pseudo-terminal; the connection carries the
// terminal's input and output. The returned session id is for Resize.
func (c Client) Pty(ctx context.Context, e Exec) (net.Conn, int, error) {
	e.Tty = true
	return c.stream(ctx, e)
}

// Resize changes a pty session's window size.
func (c Client) Resize(ctx context.Context, session int, rows, cols uint16) error {
	_, err := c.Do(ctx, Request{Op: "winsize", Winsize: &Winsize{Session: session, Rows: rows, Cols: cols}})
	return err
}

func (c Client) stream(ctx context.Context, e Exec) (net.Conn, int, error) {
	conn, err := c.Dial(ctx, Request{Op: "stream", Exec: &e})
	if err != nil {
		return nil, 0, err
	}
	line, err := ReadLine(conn)
	var resp Response
	if err == nil {
		err = json.Unmarshal(line, &resp)
	}
	if err == nil && !resp.OK {
		err = errors.New(resp.Error)
	}
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("agent stream: %w", err)
	}
	return conn, resp.Session, nil
}

// Handler answers one guest-initiated event.
type Handler func(ctx context.Context, ev Event) (json.RawMessage, error)

// Subscribe receives guest-initiated events until ctx ends or the connection
// drops (for instance across a snapshot restore); callers re-subscribe.
func (c Client) Subscribe(ctx context.Context, h Handler) error {
	conn, err := c.Dial(ctx, Request{Op: "subscribe"})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp Response
	if json.Unmarshal(line, &resp) != nil || !resp.OK {
		return fmt.Errorf("agent subscribe: %s", line)
	}
	var mu sync.Mutex
	enc := json.NewEncoder(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		go func() {
			body, err := h(ctx, ev)
			rep := Reply{ID: ev.ID, OK: err == nil, Body: body}
			if err != nil {
				rep.Error = err.Error()
			}
			mu.Lock()
			enc.Encode(rep)
			mu.Unlock()
		}()
	}
}
