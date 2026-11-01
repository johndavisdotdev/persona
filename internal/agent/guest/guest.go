//go:build linux

// Package guest is persona-agent, PID 1 inside every persona microVM (kernel
// arg init=/sbin/persona-agent). The persona binary itself is copied into the
// image under that name, so there is no separate artifact to ship. It
//
//   - builds the root filesystem: the shared read-only image as the lower
//     layer of an overlay whose upper layer is this VM's own disk (/dev/vdb),
//   - reaps orphaned processes, as init must,
//   - serves the host over vsock (see package agent): identity after a
//     snapshot restore, exec, file writes, raw and pty streams, and a channel
//     for guest-initiated requests,
//   - and, run as a command inside the guest, sends those requests:
//     persona-agent push | pr --title T [--body B] [--draft] | request KIND [JSON]
//
// Claude Code and everything else run as ordinary children, so they can exit
// or crash without taking the VM down.
package guest

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/johndavisdev/persona/internal/agent"
)

// IsAgent reports whether this process should act as persona-agent rather
// than the persona CLI: it is init, or was invoked as persona-agent.
func IsAgent() bool {
	return os.Getpid() == 1 || filepath.Base(os.Args[0]) == "persona-agent"
}

// Main runs persona-agent and never returns.
func Main() {
	if os.Getpid() == 1 {
		log.SetOutput(os.Stdout)
		log.SetFlags(log.Lmicroseconds)
		log.SetPrefix("persona-agent: ")
		initMain()
		os.Exit(1) // unreachable: serveVsock never returns
	}
	os.Exit(cli(os.Args[1:]))
}

const usage = `usage: persona-agent push
       persona-agent pr --title TITLE [--body BODY] [--draft]
       persona-agent request KIND [JSON]

push and pr ask the host to push this instance's branch to GitHub (and open a
pull request) with the host's credentials. No other branch can be pushed.`

func cli(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "push":
		return request("push", nil)
	case "pr":
		fs := flag.NewFlagSet("pr", flag.ContinueOnError)
		title := fs.String("title", "", "pull request title (required)")
		body := fs.String("body", "", "pull request body")
		draft := fs.Bool("draft", false, "open as a draft")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *title == "" {
			fmt.Fprintln(os.Stderr, "persona-agent pr: --title is required")
			return 2
		}
		b, _ := json.Marshal(agent.PRRequest{Title: *title, Body: *body, Draft: *draft})
		return request("pr", b)
	case "request":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		var body []byte
		if len(args) > 2 {
			if !json.Valid([]byte(args[2])) {
				fmt.Fprintln(os.Stderr, "persona-agent: body must be JSON")
				return 2
			}
			body = []byte(args[2])
		}
		return request(args[1], body)
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}

// request sends a guest-initiated event to the host via the local agent.
func request(kind string, body []byte) int {
	if body == nil {
		body = []byte("null")
	}
	conn, err := net.Dial("unix", agent.GuestSocket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "persona-agent:", err)
		return 1
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(agent.Event{Kind: kind, Body: body}); err != nil {
		fmt.Fprintln(os.Stderr, "persona-agent:", err)
		return 1
	}
	var rep agent.Reply
	if err := json.NewDecoder(conn).Decode(&rep); err != nil {
		fmt.Fprintln(os.Stderr, "persona-agent:", err)
		return 1
	}
	if len(rep.Body) > 0 && string(rep.Body) != "null" {
		fmt.Println(string(rep.Body))
	}
	if !rep.OK {
		fmt.Fprintln(os.Stderr, "persona-agent:", rep.Error)
		return 1
	}
	return 0
}

func initMain() {
	if err := setupRoot(); err != nil {
		log.Printf("overlay root unavailable, running on the read-only image: %v", err)
	}
	mountSystem()
	coldBootNetwork()
	go reap()
	go serveGuest()
	log.Printf("ready")
	serveVsock() // never returns: PID 1 exiting panics the kernel
}
