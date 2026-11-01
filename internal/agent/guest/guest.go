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
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

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

const usage = `usage: persona-agent push [--force]
       persona-agent pr --title TITLE [--body BODY] [--draft] [--force]
       persona-agent fetch [BRANCH...]
       persona-agent request KIND [JSON]

push and pr ask the host to push this instance's branch to GitHub (and open a
pull request) with the host's credentials. No other branch can be pushed.
--force replaces the branch's history on the host and GitHub (after a rebase);
without it, only new commits on top are accepted.

fetch brings branches of the host's repository into this one as host/BRANCH
(read-only copies: rebase or merge from them). Without a branch, it brings the
branch this instance's pull requests target. A branch can be a host branch
("main") or one of the host's remote-tracking branches ("origin/main"), as the
host last fetched it.`

func cli(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "push":
		fs := flag.NewFlagSet("push", flag.ContinueOnError)
		force := fs.Bool("force", false, "replace the branch's history (after a rebase)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		b, _ := json.Marshal(agent.PushRequest{Force: *force})
		return request("push", b)
	case "pr":
		fs := flag.NewFlagSet("pr", flag.ContinueOnError)
		title := fs.String("title", "", "pull request title (required)")
		body := fs.String("body", "", "pull request body")
		draft := fs.Bool("draft", false, "open as a draft")
		force := fs.Bool("force", false, "replace the branch's history (after a rebase)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *title == "" {
			fmt.Fprintln(os.Stderr, "persona-agent pr: --title is required")
			return 2
		}
		b, _ := json.Marshal(agent.PRRequest{Title: *title, Body: *body, Draft: *draft, Force: *force})
		return request("pr", b)
	case "fetch":
		b, _ := json.Marshal(agent.FetchRequest{Branches: args[1:]})
		rep, err := call("fetch", b)
		if err != nil {
			fmt.Fprintln(os.Stderr, "persona-agent:", err)
			return 1
		}
		if !rep.OK {
			fmt.Fprintln(os.Stderr, "persona-agent:", rep.Error)
			return 1
		}
		var res agent.FetchResult
		if err := json.Unmarshal(rep.Body, &res); err != nil {
			fmt.Fprintln(os.Stderr, "persona-agent:", err)
			return 1
		}
		for _, r := range res.Refs {
			fmt.Printf("%s\t%s\t(host branch %s)\n", r.Ref, short(r.Commit), r.Branch)
		}
		return 0
	case "status":
		return status()
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

// status forwards status events (JSON lines on stdin) to the host. It never
// fails loudly: it runs from harness hooks.
func status() int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			continue
		}
		conn, err := net.DialTimeout("unix", agent.GuestSocket, time.Second)
		if err != nil {
			return 0
		}
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		json.NewEncoder(conn).Encode(agent.Event{Kind: agent.KindStatus, Body: append([]byte{}, line...)})
		var rep agent.Reply
		json.NewDecoder(conn).Decode(&rep)
		conn.Close()
	}
	return 0
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// call sends a guest-initiated event to the host via the local agent and
// returns its reply.
func call(kind string, body []byte) (agent.Reply, error) {
	conn, err := net.Dial("unix", agent.GuestSocket)
	if err != nil {
		return agent.Reply{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(agent.Event{Kind: kind, Body: body}); err != nil {
		return agent.Reply{}, err
	}
	var rep agent.Reply
	err = json.NewDecoder(conn).Decode(&rep)
	return rep, err
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
	go sendStatus()
	log.Printf("ready")
	serveVsock() // never returns: PID 1 exiting panics the kernel
}
