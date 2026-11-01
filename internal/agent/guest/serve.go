//go:build linux

package guest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johndavisdev/persona/internal/agent"
	"golang.org/x/sys/unix"
)

// serveVsock accepts host connections forever, re-listening if the vsock
// transport is reset (as happens across a snapshot restore).
func serveVsock() {
	for {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: agent.Port})
		}
		if err == nil {
			err = unix.Listen(fd, 64)
		}
		for err == nil {
			var nfd int
			nfd, _, err = unix.Accept4(fd, unix.SOCK_CLOEXEC)
			if err == nil {
				go handle(os.NewFile(uintptr(nfd), "vsock"))
			} else if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
				err = nil
			}
		}
		log.Printf("vsock listener: %v; retrying", err)
		if fd > 0 {
			unix.Close(fd)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func reply(conn *os.File, r agent.Response) {
	b, _ := json.Marshal(r)
	conn.Write(append(b, '\n'))
}

func fail(conn *os.File, err error) { reply(conn, agent.Response{Error: err.Error()}) }

func handle(conn *os.File) {
	line, err := agent.ReadLine(conn)
	if err != nil {
		conn.Close()
		return
	}
	var req agent.Request
	if err := json.Unmarshal(line, &req); err != nil {
		fail(conn, err)
		conn.Close()
		return
	}
	switch req.Op {
	case "subscribe":
		subscribe(conn) // owns conn from here
		return
	case "stream":
		stream(conn, req.Exec)
		return
	}
	defer conn.Close()
	switch req.Op {
	case "ping":
		reply(conn, agent.Response{OK: true})
	case "identify":
		if req.Identity == nil {
			fail(conn, errors.New("identity missing"))
		} else if err := identify(*req.Identity); err != nil {
			fail(conn, err)
		} else {
			reply(conn, agent.Response{OK: true})
		}
	case "quiesce":
		if err := quiesce(); err != nil {
			fail(conn, err)
		} else {
			reply(conn, agent.Response{OK: true})
		}
	case "exec":
		if req.Exec == nil {
			fail(conn, errors.New("exec missing"))
		} else {
			reply(conn, run(req.Exec))
		}
	case "winsize":
		if req.Winsize == nil {
			fail(conn, errors.New("winsize missing"))
		} else if err := resize(*req.Winsize); err != nil {
			fail(conn, err)
		} else {
			reply(conn, agent.Response{OK: true})
		}
	case "write":
		if req.Write == nil {
			fail(conn, errors.New("write missing"))
		} else if err := writeFile(*req.Write); err != nil {
			fail(conn, err)
		} else {
			reply(conn, agent.Response{OK: true})
		}
	default:
		fail(conn, fmt.Errorf("unknown op %q", req.Op))
	}
}

// stream hands the connection to a program as its stdin and stdout.
func stream(conn *os.File, e *agent.Exec) {
	defer conn.Close()
	if e == nil {
		fail(conn, errors.New("exec missing"))
		return
	}
	if e.Tty {
		ptyStream(conn, e)
		return
	}
	reply(conn, agent.Response{OK: true})
	if _, _, err := start(e, []*os.File{conn, conn, os.Stdout}, false, false); err != nil {
		log.Printf("stream %v: %v", e.Argv, err)
	}
}

func writeFile(w agent.Write) error {
	if !filepath.IsAbs(w.Path) {
		return fmt.Errorf("path must be absolute")
	}
	acct, err := lookupUser(w.User)
	if err != nil {
		return err
	}
	uid, gid := acct.uid, acct.gid
	if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.Path), ".persona-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(w.Data)
	if err == nil {
		err = tmp.Chmod(os.FileMode(w.Mode & 0o7777))
	}
	if err == nil {
		err = tmp.Chown(uid, gid)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), w.Path)
}

// --- guest-initiated requests ---

type subscriber struct {
	conn    *os.File
	mu      sync.Mutex // serializes writes
	pending sync.Map   // id -> chan agent.Reply
}

var (
	subMu  sync.Mutex
	sub    *subscriber
	nextID atomic.Uint64
)

func subscribe(conn *os.File) {
	s := &subscriber{conn: conn}
	reply(conn, agent.Response{OK: true})
	subMu.Lock()
	if sub != nil {
		sub.conn.Close()
	}
	sub = s
	subMu.Unlock()

	dec := json.NewDecoder(conn)
	for {
		var rep agent.Reply
		if err := dec.Decode(&rep); err != nil {
			break
		}
		if ch, ok := s.pending.LoadAndDelete(rep.ID); ok {
			ch.(chan agent.Reply) <- rep
		}
	}
	subMu.Lock()
	if sub == s {
		sub = nil
	}
	subMu.Unlock()
	conn.Close()
	s.pending.Range(func(k, v any) bool {
		v.(chan agent.Reply) <- agent.Reply{ID: k.(uint64), Error: "host disconnected"}
		return true
	})
}

// forward sends ev to the host subscriber and waits for its reply.
func forward(ev agent.Event) agent.Reply {
	subMu.Lock()
	s := sub
	subMu.Unlock()
	if s == nil {
		return agent.Reply{Error: "no host broker connected"}
	}
	ev.ID = nextID.Add(1)
	ch := make(chan agent.Reply, 1)
	s.pending.Store(ev.ID, ch)
	b, _ := json.Marshal(ev)
	s.mu.Lock()
	_, err := s.conn.Write(append(b, '\n'))
	s.mu.Unlock()
	if err != nil {
		s.pending.Delete(ev.ID)
		return agent.Reply{Error: "host disconnected"}
	}
	select {
	case rep := <-ch:
		return rep
	case <-time.After(30 * time.Minute):
		s.pending.Delete(ev.ID)
		return agent.Reply{Error: "timed out waiting for host"}
	}
}

// serveGuest accepts requests from guest programs on a local socket.
func serveGuest() {
	_ = os.Remove(agent.GuestSocket)
	l, err := net.Listen("unix", agent.GuestSocket)
	if err != nil {
		log.Printf("guest socket: %v", err)
		return
	}
	_ = os.Chmod(agent.GuestSocket, 0o666)
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			var ev agent.Event
			if err := json.NewDecoder(c).Decode(&ev); err != nil {
				return
			}
			if ev.Kind == agent.KindStatus {
				queueStatus(ev)
				json.NewEncoder(c).Encode(agent.Reply{OK: true})
				return
			}
			json.NewEncoder(c).Encode(forward(ev))
		}()
	}
}

// Status events are fire-and-forget: hooks must never wait on the host. They
// queue here (bounded) and go to the broker whenever one is subscribed, so
// events from before it connected aren't lost.
var (
	statusMu    sync.Mutex
	statusQueue []agent.Event
	statusWake  = make(chan struct{}, 1)
)

const statusQueueMax = 500

func queueStatus(ev agent.Event) {
	statusMu.Lock()
	statusQueue = append(statusQueue, ev)
	if len(statusQueue) > statusQueueMax {
		statusQueue = statusQueue[len(statusQueue)-statusQueueMax:]
	}
	statusMu.Unlock()
	select {
	case statusWake <- struct{}{}:
	default:
	}
}

// sendStatus delivers queued status events to the current subscriber.
func sendStatus() {
	for {
		select {
		case <-statusWake:
		case <-time.After(time.Second):
		}
		subMu.Lock()
		s := sub
		subMu.Unlock()
		if s == nil {
			continue
		}
		statusMu.Lock()
		batch := statusQueue
		statusQueue = nil
		statusMu.Unlock()
		for i, ev := range batch {
			ev.ID = nextID.Add(1)
			b, _ := json.Marshal(ev)
			s.mu.Lock()
			_, err := s.conn.Write(append(b, '\n'))
			s.mu.Unlock()
			if err != nil { // subscriber gone: requeue the rest
				statusMu.Lock()
				statusQueue = append(batch[i:], statusQueue...)
				statusMu.Unlock()
				break
			}
		}
	}
}
