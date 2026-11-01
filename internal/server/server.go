// Package server is persona's web UI: an HTTP/3 server whose page talks to it
// over WebTransport. One session per browser tab carries
//
//   - a control stream: newline-delimited JSON requests and responses, and
//     the state (spawns, namespaces, personas, port forwards) pushed whenever
//     it changes;
//   - a stream per terminal (a spawn's Claude Code session) or job (persona
//     new, build, spawn), each a program run under a pty; see stream.go.
//
// What the server can do comes from Actions (implemented by package cmd), so
// it behaves exactly as the CLI does.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// dist is the web app (web/, React) as built by `make web`, embedded in the
// binary. --web serves another build from disk instead.
//
//go:embed all:dist
var webFS embed.FS

// Actions is what the UI can do; package cmd implements it on top of the
// runtime, as the CLI commands do.
type Actions interface {
	// State is the current picture of spawns, namespaces, personas and forwards.
	State(ctx context.Context) State
	// Do runs a quick request (namespaces, firewall rules, stop/start/kill).
	Do(ctx context.Context, op string, args json.RawMessage) (any, error)
	// Attach is the command that attaches a terminal to a spawn's session.
	Attach(ctx context.Context, spawn string) (*exec.Cmd, error)
	// Job is the command for a long-running job (new, build, spawn).
	Job(ctx context.Context, kind string, args json.RawMessage) (*exec.Cmd, error)
	// Notices delivers what agents report to the host as it happens (work
	// finished, input needed), until ctx ends.
	Notices(ctx context.Context) <-chan Notice
}

// Notice is an agent reporting to the host through its beacon:
// report_work_finished (Kind "finished") or request_input (Kind "input").
type Notice struct {
	Spawn     string    `json:"spawn"`
	Persona   string    `json:"persona"`
	Namespace string    `json:"namespace"`
	Kind      string    `json:"kind"`
	Question  string    `json:"question,omitempty"`
	Options   []string  `json:"options,omitempty"`
	TS        time.Time `json:"ts"`
}

// State is what the page shows.
type State struct {
	Runtime    string        `json:"runtime"`
	Namespaces []string      `json:"namespaces"`
	Spawns     []Spawn       `json:"spawns"`
	Personas   []PersonaInfo `json:"personas"`
	Forwards   []Forward     `json:"forwards"`
	Features   Features      `json:"features"`
	// HostAddresses are the host's own IPv4 addresses a port forward can
	// listen on, besides localhost.
	HostAddresses []string `json:"host_addresses"`
	Errors        []string `json:"errors,omitempty"`
}

// Features says what the runtime supports.
type Features struct {
	Namespaces bool `json:"namespaces"`
	Firewall   bool `json:"firewall"`
}

// Spawn is one spawn.
type Spawn struct {
	Name      string `json:"name"`      // the spawn's own name
	Container string `json:"container"` // the runtime's name
	Persona   string `json:"persona"`
	Namespace string `json:"namespace"`
	State     string `json:"state"`
	Status    string `json:"status"`
	IP        string `json:"ip,omitempty"`
	CGNAT     string `json:"cgnat,omitempty"`
	Created   string `json:"created"`
}

// PersonaInfo is one persona.
type PersonaInfo struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Harness string `json:"harness"`
	Built   bool   `json:"built"`
}

// Forward is one port forward.
type Forward struct {
	ID        int    `json:"id"`
	Spawn     string `json:"spawn"`
	Namespace string `json:"namespace"`
	Proto     string `json:"proto"`
	HostIP    string `json:"host_ip"`
	HostPort  int    `json:"host_port"`
	Port      int    `json:"port"`
}

// Server serves the UI.
type Server struct {
	Addr    string // host:port, TCP (the page) and UDP (HTTP/3)
	Token   string // required to open a session
	Actions Actions
	Log     *log.Logger
	// TLS: a certificate from the user (CertFile, KeyFile), or a self-signed
	// one (CertDir) that rotates and is trusted by hash. PlainHTTP serves the
	// page itself without TLS (fine on loopback, a secure context).
	CertFile, KeyFile string
	CertDir           string
	PlainHTTP         bool
	// WebDir, if set, is served instead of the embedded web app: a single-page
	// app's build output (unknown paths get its index.html).
	WebDir string
	// AllowOrigins are other origins whose pages may connect (e.g. a Vite dev
	// server); the page's own origin always may.
	AllowOrigins []string

	certs   *certSource
	hub     *hub
	notices *noticeHub
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if s.Log == nil {
		s.Log = log.Default()
	}
	certs, err := newCertSource(s.CertFile, s.KeyFile, s.CertDir, s.Addr)
	if err != nil {
		return err
	}
	s.certs = certs
	s.hub = newHub(ctx, s.Actions)
	s.notices = newNoticeHub(ctx, s.Actions.Notices(ctx))

	h3 := &http3.Server{
		Addr:       s.Addr,
		TLSConfig:  http3.ConfigureTLSConfig(certs.tlsConfig()),
		QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, KeepAlivePeriod: 15 * time.Second},
	}
	webtransport.ConfigureHTTP3Server(h3)
	wt := &webtransport.Server{H3: h3, CheckOrigin: s.checkOrigin}

	var static fs.FS
	if s.WebDir != "" {
		static = os.DirFS(s.WebDir)
	} else {
		static, _ = fs.Sub(webFS, "dist")
	}
	mux := http.NewServeMux()
	mux.Handle("/", noCache(spa(static)))
	mux.HandleFunc("/config.json", s.cors(s.config))
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.Token)) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			s.Log.Printf("webtransport upgrade: %v", err)
			return
		}
		go s.serveSession(ctx, sess)
	})
	h3.Handler = mux

	tcp := &http.Server{Addr: s.Addr, ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = h3.SetQUICHeaders(w.Header()) // Alt-Svc: h3
			mux.ServeHTTP(w, r)
		})}
	if !s.PlainHTTP {
		tcp.TLSConfig = certs.tlsConfig()
	}

	errc := make(chan error, 2)
	go func() { errc <- wt.ListenAndServe() }()
	go func() {
		if s.PlainHTTP {
			errc <- tcp.ListenAndServe()
		} else {
			errc <- tcp.ListenAndServeTLS("", "")
		}
	}()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tcp.Shutdown(sctx)
		_ = wt.Close()
		return nil
	case err := <-errc:
		_ = tcp.Close()
		_ = wt.Close()
		return err
	}
}

// config tells the page where WebTransport is, and the certificate hash to
// trust when it is self-signed.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, portOf(s.Addr))
	}
	cfg := map[string]any{"url": "https://" + host + "/wt"}
	if h := s.certs.hash(); h != "" {
		cfg["certHash"] = h
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(cfg)
}

// checkOrigin allows the page's own origin (same host:port) and AllowOrigins.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || s.allowed(origin) {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) allowed(origin string) bool {
	for _, o := range s.AllowOrigins {
		if strings.EqualFold(strings.TrimSuffix(o, "/"), origin) {
			return true
		}
	}
	return false
}

// cors lets AllowOrigins fetch h (config.json) from their pages.
func (s *Server) cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && s.allowed(o) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
		}
		h(w, r)
	}
}

// spa serves files from root; a path with no file gets index.html, so a
// single-page app's client-side routes load.
func spa(root fs.FS) http.Handler {
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if _, err := fs.Stat(root, p); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

// serveSession accepts a browser session's streams until it ends.
func (s *Server) serveSession(ctx context.Context, sess *webtransport.Session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sess.Context().Done()
		cancel()
	}()
	for {
		st, err := sess.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.serveStream(ctx, st)
	}
}

// serveStream reads a stream's header (one JSON line) and serves it.
func (s *Server) serveStream(ctx context.Context, st *webtransport.Stream) {
	defer st.Close()
	line, err := readLine(st, 64<<10)
	if err != nil {
		return
	}
	var h struct {
		Type   string          `json:"type"`
		Spawn  string          `json:"spawn"`
		Kind   string          `json:"kind"`
		Args   json.RawMessage `json:"args"`
		Cols   uint16          `json:"cols"`
		Rows   uint16          `json:"rows"`
		Detach bool            `json:"detach"`
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return
	}
	switch h.Type {
	case "control":
		s.serveControl(ctx, st)
	case "attach":
		cmd, err := s.Actions.Attach(ctx, h.Spawn)
		if err != nil {
			writeExit(st, -1, err.Error())
			return
		}
		runPTY(ctx, st, cmd, h.Cols, h.Rows, true)
	case "job":
		cmd, err := s.Actions.Job(ctx, h.Kind, h.Args)
		if err != nil {
			writeExit(st, -1, err.Error())
			return
		}
		runPTY(ctx, st, cmd, h.Cols, h.Rows, false)
	}
}

// serveControl answers requests and pushes state until the stream ends.
func (s *Server) serveControl(ctx context.Context, st *webtransport.Stream) {
	var mu sync.Mutex
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		_ = st.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err = st.Write(append(b, '\n'))
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := s.hub.subscribe(ctx)
	go func() {
		for state := range updates {
			if send(map[string]any{"event": "state", "data": state}) != nil {
				cancel()
				return
			}
		}
	}()
	notices := s.notices.subscribe(ctx)
	go func() {
		for n := range notices {
			if send(map[string]any{"event": "notice", "data": n}) != nil {
				cancel()
				return
			}
		}
	}()
	for {
		line, err := readLine(st, 1<<20)
		if err != nil {
			return
		}
		var req struct {
			ID   int             `json:"id"`
			Op   string          `json:"op"`
			Args json.RawMessage `json:"args"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		go func() {
			res, err := s.Actions.Do(ctx, req.Op, req.Args)
			if err != nil {
				_ = send(map[string]any{"id": req.ID, "ok": false, "error": err.Error()})
			} else {
				_ = send(map[string]any{"id": req.ID, "ok": true, "result": res})
			}
			s.hub.poke() // reflect the change right away
		}()
	}
}

// readLine reads up to a newline, a byte at a time (nothing past it is lost).
func readLine(r interface{ Read([]byte) (int, error) }, max int) ([]byte, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < max {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return b, nil
			}
			b = append(b, one[0])
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("line too long")
}

// URL is how to open the UI.
func (s *Server) URL(host string) string {
	scheme := "https"
	if s.PlainHTTP {
		scheme = "http"
	}
	if host == "" {
		host, _, _ = net.SplitHostPort(s.Addr)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
	}
	return fmt.Sprintf("%s://%s/#token=%s", scheme, net.JoinHostPort(strings.Trim(host, "[]"), portOf(s.Addr)), s.Token)
}
