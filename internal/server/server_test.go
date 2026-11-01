package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

type fakeActions struct{}

func (fakeActions) State(context.Context) State {
	return State{Runtime: "test", Namespaces: []string{"default"}, Spawns: []Spawn{{Name: "jill", Namespace: "default", State: "running"}}}
}

func (fakeActions) Do(_ context.Context, op string, args json.RawMessage) (any, error) {
	if op == "echo" {
		return json.RawMessage(args), nil
	}
	return nil, fmt.Errorf("unknown request %q", op)
}

func (fakeActions) Attach(ctx context.Context, spawn string) (*exec.Cmd, error) {
	// An interactive "terminal": echoes lines and reports its size.
	return exec.CommandContext(ctx, "sh", "-c", `stty size; read l; echo "got:$l"; stty size`), nil
}

var testNotices = make(chan Notice, 1)

func (fakeActions) Notices(context.Context) <-chan Notice { return testNotices }

func (fakeActions) Job(ctx context.Context, kind string, _ json.RawMessage) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "sh", "-c", "echo building; exit 3"), nil
}

func freePort(t *testing.T) string {
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Listen("tcp", l.LocalAddr().String())
	if err != nil {
		t.Skip("port in use for TCP")
	}
	c.Close()
	return l.LocalAddr().String()
}

func startServer(t *testing.T) (*Server, string) {
	addr := freePort(t)
	s := &Server{Addr: addr, Token: "secret-token-0123456789abcdef0123", Actions: fakeActions{}, CertDir: t.TempDir(), PlainHTTP: true}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()
	for i := 0; i < 100; i++ {
		if r, err := http.Get("http://" + addr + "/config.json"); err == nil {
			r.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s, addr
}

func dial(t *testing.T, addr, token string) (*webtransport.Session, error) {
	r, err := http.Get("http://" + addr + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ URL, CertHash string }
	json.NewDecoder(r.Body).Decode(&cfg)
	r.Body.Close()
	if cfg.CertHash == "" || !strings.HasSuffix(cfg.URL, "/wt") {
		t.Fatalf("config = %+v", cfg)
	}
	tlsConf := &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
		if base64.StdEncoding.EncodeToString(sum[:]) != cfg.CertHash {
			return fmt.Errorf("certificate hash mismatch")
		}
		return nil
	}}
	d := &webtransport.Transport{TLSClientConfig: tlsConf, QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Origin": {"http://" + addr}}
	rsp, sess, err := d.Dial(ctx, cfg.URL+"?token="+token, hdr)
	if err != nil {
		return nil, err
	}
	if rsp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", rsp.StatusCode)
	}
	return sess, nil
}

func TestServerRejectsBadToken(t *testing.T) {
	_, addr := startServer(t)
	if _, err := dial(t, addr, "wrong"); err == nil {
		t.Fatal("dialed with a wrong token")
	}
}

func TestServerControlAndStreams(t *testing.T) {
	s, addr := startServer(t)
	sess, err := dial(t, addr, s.Token)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Control: the state arrives at once; requests get answers.
	st, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(st, `{"type":"control"}`)
	fmt.Fprintln(st, `{"id":7,"op":"echo","args":{"x":1}}`)
	fmt.Fprintln(st, `{"id":8,"op":"nope"}`)
	sc := bufio.NewScanner(st)
	got := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(got) < 4 {
		lines := make(chan string, 1)
		go func() {
			if sc.Scan() {
				lines <- sc.Text()
			} else {
				close(lines)
			}
		}()
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("control stream ended; got %v", got)
			}
			switch {
			case strings.Contains(l, `"event":"state"`) && strings.Contains(l, `"jill"`):
				if !got["state"] { // subscribed now: a notice reaches this page
					testNotices <- Notice{Spawn: "jill", Kind: "input", Question: "Deploy?"}
				}
				got["state"] = true
			case strings.Contains(l, `"event":"notice"`) && strings.Contains(l, `"Deploy?"`):
				got["notice"] = true
			case strings.Contains(l, `"id":7`) && strings.Contains(l, `"ok":true`) && strings.Contains(l, `"x":1`):
				got["echo"] = true
			case strings.Contains(l, `"id":8`) && strings.Contains(l, `"ok":false`):
				got["error"] = true
			}
		case <-deadline:
			t.Fatalf("timed out; got %v", got)
		}
	}

	// A job: its output, then its exit status.
	js, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(js, `{"type":"job","kind":"build","cols":80,"rows":24}`)
	out, code := readUntilExit(t, js)
	if !strings.Contains(out, "building") || code != 3 {
		t.Errorf("job: output %q, exit %d", out, code)
	}

	// A terminal: sized, takes input, resizes.
	as, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(as, `{"type":"attach","spawn":"jill","cols":100,"rows":30}`)
	fw := &frameWriter{w: as}
	time.Sleep(300 * time.Millisecond)
	fw.write(frameResize, []byte(`{"cols":90,"rows":20}`))
	time.Sleep(100 * time.Millisecond)
	fw.write(frameData, []byte("hello\r"))
	out, code = readUntilExit(t, as)
	if !strings.Contains(out, "30 100") || !strings.Contains(out, "got:hello") || !strings.Contains(out, "20 90") || code != 0 {
		t.Errorf("terminal: output %q, exit %d", out, code)
	}
}

func readUntilExit(t *testing.T, st *webtransport.Stream) (string, int) {
	t.Helper()
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	var out strings.Builder
	for {
		typ, b, err := readFrame(st)
		if err != nil {
			t.Fatalf("reading frames: %v (so far %q)", err, out.String())
		}
		switch typ {
		case frameData:
			out.Write(b)
		case frameExit:
			var ex struct{ Code int }
			json.Unmarshal(b, &ex)
			return out.String(), ex.Code
		}
	}
}

func TestSPAAndOrigins(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/index.html", []byte("app-index"), 0o644)
	os.MkdirAll(dir+"/assets", 0o755)
	os.WriteFile(dir+"/assets/app.js", []byte("app-js"), 0o644)
	h := spa(os.DirFS(dir))
	for path, want := range map[string]string{"/": "app-index", "/assets/app.js": "app-js", "/spawns/jill": "app-index"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %q, want %q", path, rec.Body.String(), want)
		}
	}

	s := &Server{AllowOrigins: []string{"http://172.16.1.100:5173"}}
	req := func(origin, host string) *http.Request {
		r := httptest.NewRequest("GET", "/wt", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	if !s.checkOrigin(req("http://127.0.0.1:7443", "127.0.0.1:7443")) {
		t.Error("the page's own origin was refused")
	}
	if !s.checkOrigin(req("http://172.16.1.100:5173", "127.0.0.1:7443")) {
		t.Error("an allowed origin was refused")
	}
	if s.checkOrigin(req("https://evil.example", "127.0.0.1:7443")) {
		t.Error("a foreign origin was accepted")
	}
}
