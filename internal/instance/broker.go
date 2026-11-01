//go:build linux

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/johndavisdev/persona/internal/agent"
	"github.com/johndavisdev/persona/internal/config"
	"github.com/johndavisdev/persona/internal/microvm"
	"github.com/johndavisdev/persona/internal/status"
)

// Broker serves guest-initiated requests for every running microVM spawn:
// status events (stored for persona manage), and push, pr and fetch for
// spawns with a git workspace. It runs once per user, holds the
// only GitHub credentials involved (the host's git and gh), and only ever
// pushes an instance's own branch.
type Broker struct {
	GH  string // gh binary; default "gh"
	Log *log.Logger
	// Idle stops the broker after this long with no instances; 0 never stops.
	Idle time.Duration

	mu   sync.Mutex
	subs map[string]context.CancelFunc // by instance name
	busy map[string]*sync.Mutex        // one push at a time per instance
}

func brokerLock() string { return filepath.Join(config.Home(), "broker.lock") }

// Run serves until ctx ends. Only one broker per user runs at a time.
func (b *Broker) Run(ctx context.Context) error {
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(brokerLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another broker is already running")
	}
	if b.Log == nil {
		b.Log = log.New(os.Stderr, "persona broker: ", log.LstdFlags)
	}
	b.subs, b.busy = map[string]context.CancelFunc{}, map[string]*sync.Mutex{}
	b.Log.Printf("started")
	self, _ := os.Executable()
	selfID := fileID(self)
	idleSince := time.Now()
	for {
		// An upgrade replaced the binary: carry on as the new one, which
		// knows any new requests. The lock is close-on-exec, so the new
		// process takes it over.
		if selfID != "" && fileID(self) != selfID {
			b.Log.Printf("%s was replaced; restarting", self)
			if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
				b.Log.Printf("restart: %v", err)
				selfID = fileID(self)
			}
		}
		n := b.scan(ctx)
		if n > 0 {
			idleSince = time.Now()
		} else if b.Idle > 0 && time.Since(idleSince) > b.Idle {
			b.Log.Printf("no instances for %v, exiting", b.Idle)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// fileID identifies a file's current content: its inode and modification
// time ("" if it can't be read).
func fileID(path string) string {
	var st syscall.Stat_t
	if path == "" || syscall.Stat(path, &st) != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Ino, st.Mtim.Nano())
}

// scan subscribes to new instances and drops ones that went away.
func (b *Broker) scan(ctx context.Context) int {
	recs, _ := allRecords()
	live := map[string]bool{}
	for _, r := range recs {
		if _, ok := microvm.Lookup(r.VMID); !ok {
			continue
		}
		live[r.Name] = true
		b.mu.Lock()
		if _, ok := b.subs[r.Name]; !ok {
			sctx, cancel := context.WithCancel(ctx)
			b.subs[r.Name] = cancel
			b.busy[r.Name] = &sync.Mutex{}
			go b.serve(sctx, r.Name)
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	for name, cancel := range b.subs {
		if !live[name] {
			cancel()
			delete(b.subs, name)
			delete(b.busy, name)
		}
	}
	b.mu.Unlock()
	return len(live)
}

func (b *Broker) serve(ctx context.Context, name string) {
	for ctx.Err() == nil {
		rec, err := loadRecord(name)
		if err != nil {
			return
		}
		vm, ok := microvm.Lookup(rec.VMID)
		if !ok {
			return
		}
		err = vm.Agent.Subscribe(ctx, func(ctx context.Context, ev agent.Event) (json.RawMessage, error) {
			return b.handle(ctx, name, ev)
		})
		if ctx.Err() == nil {
			time.Sleep(500 * time.Millisecond) // e.g. agent restarting; resubscribe
		}
		_ = err
	}
}

func (b *Broker) handle(ctx context.Context, name string, ev agent.Event) (json.RawMessage, error) {
	if ev.Kind == agent.KindStatus {
		rec, err := loadRecord(name)
		if err != nil {
			return nil, err
		}
		return nil, status.Append(rec.Instance, ev.Body)
	}
	if ev.Kind == agent.KindBeacon {
		raw, err := beaconStatus(ev.Body)
		if err != nil {
			return nil, err
		}
		rec, err := loadRecord(name)
		if err != nil {
			return nil, err
		}
		b.Log.Printf("%s: beacon %s", name, ev.Body)
		return nil, status.Append(rec.Instance, raw)
	}
	b.mu.Lock()
	mu := b.busy[name]
	b.mu.Unlock()
	if mu == nil {
		return nil, errors.New("instance is shutting down")
	}
	mu.Lock()
	defer mu.Unlock()
	rec, err := loadRecord(name)
	if err != nil {
		return nil, err
	}
	vm, ok := microvm.Lookup(rec.VMID)
	if !ok {
		return nil, errors.New("instance is not running")
	}
	if ev.Kind == agent.KindFetch {
		var req agent.FetchRequest
		if len(ev.Body) > 0 && string(ev.Body) != "null" {
			if err := json.Unmarshal(ev.Body, &req); err != nil {
				return nil, errors.New("fetch: bad request")
			}
		}
		res, err := fetchIntoVM(ctx, rec, vm, req.Branches)
		if err != nil {
			b.Log.Printf("%s: fetch failed: %v", name, err)
			return nil, err
		}
		b.Log.Printf("%s: fetch %v", name, res.Refs)
		return json.Marshal(res)
	}
	var res agent.PushResult
	switch ev.Kind {
	case agent.KindPush:
		var req agent.PushRequest
		if len(ev.Body) > 0 && string(ev.Body) != "null" {
			if err := json.Unmarshal(ev.Body, &req); err != nil {
				return nil, errors.New("push: bad request")
			}
		}
		res, err = b.push(ctx, rec, vm, req.Force)
	case agent.KindPR:
		var req agent.PRRequest
		if err := json.Unmarshal(ev.Body, &req); err != nil || strings.TrimSpace(req.Title) == "" {
			return nil, errors.New("pr needs a title")
		}
		if res, err = b.push(ctx, rec, vm, req.Force); err == nil {
			res.PR, err = b.pr(ctx, rec, req)
		}
	default:
		return nil, fmt.Errorf("unsupported request %q", ev.Kind)
	}
	if err != nil {
		b.Log.Printf("%s: %s failed: %v", name, ev.Kind, err)
		return nil, err
	}
	b.Log.Printf("%s: %s %s at %s %s", name, ev.Kind, res.Branch, res.Commit, res.PR)
	return json.Marshal(res)
}

func (b *Broker) push(ctx context.Context, rec *record, vm *microvm.VM, force bool) (agent.PushResult, error) {
	mode := fastForward
	if force {
		mode = keepChanges
	}
	commit, err := fetchBranch(ctx, rec, vm, mode)
	if err != nil {
		return agent.PushResult{}, err
	}
	remote, err := git(ctx, rec.Repo, "remote", "get-url", "--push", "origin")
	if err != nil {
		return agent.PushResult{}, errors.New("the host repository has no origin remote to push to")
	}
	ref := "refs/heads/" + rec.Branch
	args := []string{"push", "-q"}
	if force {
		args = append(args, "--force-with-lease") // only over what this host last saw there
	}
	if _, err := git(ctx, rec.Repo, append(args, "origin", ref+":"+ref)...); err != nil {
		return agent.PushResult{}, err
	}
	return agent.PushResult{Branch: rec.Branch, Commit: commit, Remote: remote}, nil
}

func (b *Broker) pr(ctx context.Context, rec *record, req agent.PRRequest) (string, error) {
	gh := b.GH
	if gh == "" {
		gh = "gh"
	}
	// --flag=value keeps titles starting with "-" from being read as flags.
	args := []string{"pr", "create", "--head=" + rec.Branch, "--base=" + rec.Base, "--title=" + req.Title, "--body=" + req.Body}
	if req.Draft {
		args = append(args, "--draft")
	}
	cmd := exec.CommandContext(ctx, gh, args...)
	cmd.Dir = rec.Repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			view := exec.CommandContext(ctx, gh, "pr", "view", rec.Branch, "--json", "url", "--jq", ".url")
			view.Dir = rec.Repo
			if u, verr := view.Output(); verr == nil {
				return strings.TrimSpace(string(u)), nil
			}
		}
		return "", fmt.Errorf("gh pr create: %s", strings.TrimSpace(string(out)))
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "", errors.New("gh pr create printed no URL")
	}
	return lines[len(lines)-1], nil
}

// EnsureBroker starts the broker in the background unless one is running.
func EnsureBroker(self string) error {
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(brokerLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil // held: a broker is running
	}
	lf.Close() // released; the new broker takes it
	logf, err := os.OpenFile(filepath.Join(config.Home(), "broker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "broker")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
