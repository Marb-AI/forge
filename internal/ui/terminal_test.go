package ui

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Marb-AI/forge/forge"
)

// fakeTerm is a terminal with nothing behind it: no ssh, no shell, no pty. What
// the browser types lands in a buffer the test can read, and what the test says
// comes out of the stream — which is the whole of what this package does with a
// terminal now that opening one is the core's job.
type fakeTerm struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu     sync.Mutex
	typed  bytes.Buffer
	cols   uint16
	rows   uint16
	closed bool
}

func newFakeTerm() *fakeTerm {
	pr, pw := io.Pipe()
	return &fakeTerm{pr: pr, pw: pw}
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.pr.Read(p) }

func (f *fakeTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.typed.Write(p)
}

func (f *fakeTerm) Resize(cols, rows uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cols, f.rows = cols, rows
	return nil
}

func (f *fakeTerm) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	_ = f.pw.Close()
	return f.pr.Close()
}

// say makes the far end print something. It blocks until the stream reads it, so
// a test that gets past this line knows the handler is listening.
func (f *fakeTerm) say(s string) { _, _ = f.pw.Write([]byte(s)) }

func (f *fakeTerm) typedSoFar() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.typed.String()
}

func (f *fakeTerm) size() (uint16, uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols, f.rows
}

func (f *fakeTerm) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// openedTerm is one terminal a handler asked the core for.
type openedTerm struct {
	kind string
	ws   string
	cols uint16
	rows uint16
	term *fakeTerm
}

// termOpens stands in for forge.OpenTerminal, recording what it was asked for.
type termOpens struct {
	mu   sync.Mutex
	list []openedTerm
}

func (o *termOpens) open(kind, ws string, cols, rows uint16) (Terminal, error) {
	f := newFakeTerm()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, openedTerm{kind: kind, ws: ws, cols: cols, rows: rows, term: f})
	return f, nil
}

func (o *termOpens) all() []openedTerm {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]openedTerm(nil), o.list...)
}

// termServer is a UI server whose terminals are fakes, so every test below runs
// the real handlers — registry, SSE framing, input, resize — without a server to
// ssh to.
func termServer(t *testing.T) (*server, http.Handler, *termOpens) {
	t.Helper()
	s, h := testServer(t)
	opens := &termOpens{}
	s.deps.KnowsWorkspace = func(ws string) bool { return ws == "crm" }
	s.deps.OpenTerminal = opens.open
	return s, h, opens
}

// The browser asks for a kind, a workspace and a size, and every one of the four
// has to reach the core intact: a stream that opened the wrong kind would attach
// a shell where the Claude session belongs, and one that dropped the size would
// draw its first frame into the wrong rectangle.
func TestTheStreamOpensTheTerminalTheBrowserAskedFor(t *testing.T) {
	for _, c := range []struct {
		path     string
		wantKind string
		wantWS   string
	}{
		{"/api/term/crm/claude/stream?cols=100&rows=40", termClaude, "crm"},
		{"/api/term/crm/ssh/stream?cols=100&rows=40", termSSH, "crm"},
		{"/api/term/crm/host/stream?cols=100&rows=40", termHost, "crm"},
		// The local shell belongs to no workspace, so it must be asked for without
		// one — a workspace here would be a workspace the core has to ignore.
		{"/api/term/local/stream?cols=100&rows=40", forge.TermLocal, ""},
	} {
		t.Run(c.wantKind, func(t *testing.T) {
			_, h, opens := termServer(t)
			srv := httptest.NewServer(h)
			defer srv.Close()

			resp := openStream(t, srv.URL+c.path)
			defer resp.Body.Close()

			got := opens.all()
			if len(got) != 1 {
				t.Fatalf("the stream opened %d terminals, want 1", len(got))
			}
			if got[0].kind != c.wantKind || got[0].ws != c.wantWS {
				t.Errorf("opened (%q, %q), want (%q, %q)", got[0].kind, got[0].ws, c.wantKind, c.wantWS)
			}
			if got[0].cols != 100 || got[0].rows != 40 {
				t.Errorf("opened at %d×%d, want the size the browser measured, 100×40",
					got[0].cols, got[0].rows)
			}
		})
	}
}

// A workspace this client does not have, and a kind that is not a terminal, are
// both answered before anything is opened: an ssh connection to nowhere is the
// expensive way to produce a 404.
func TestNoTerminalIsOpenedForAWorkspaceOrKindThatIsNot(t *testing.T) {
	_, h, opens := termServer(t)

	for _, path := range []string{
		"/api/term/nope/claude/stream",
		"/api/term/crm/sftp/stream",
	} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:47615"+path, nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if got := opens.all(); len(got) != 0 {
		t.Errorf("%d terminal(s) were opened for requests that should never reach one: %v", len(got), got)
	}
}

// The round trip the browser makes: open the stream, type into it over the input
// endpoint, and read the output back out of the stream. This is what the unit
// tests can't see — the registry key the stream and the input agree on, the base64
// framing, and a resize reaching the terminal that is open.
func TestATerminalCarriesOutputInputAndResizeBothWays(t *testing.T) {
	_, h, opens := termServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := openStream(t, srv.URL+"/api/term/crm/claude/stream?cols=80&rows=24")
	defer resp.Body.Close()
	term := opens.all()[0].term

	// Output: raw bytes, escape codes and all, must survive SSE's line framing.
	go term.say("\x1b[32mhello\x1b[0m\nsecond line\n")
	if got, ok := readSSEUntil(t, resp.Body, "second line", 10*time.Second); !ok {
		t.Fatalf("the terminal's output never reached the browser; stream carried:\n%q", got)
	} else if !strings.Contains(got, "\x1b[32mhello\x1b[0m") {
		t.Errorf("the stream mangled the escape codes: %q", got)
	}

	// Input: base64 in the body, raw bytes out at the terminal.
	post(t, srv.URL+"/api/term/crm/claude/input",
		base64.StdEncoding.EncodeToString([]byte("ls -l\r")), http.StatusNoContent)
	if got := term.typedSoFar(); got != "ls -l\r" {
		t.Errorf("the terminal received %q, want the keystrokes the browser sent", got)
	}

	// Resize: the browser's new measurement, in the order the terminal expects.
	post(t, srv.URL+"/api/term/crm/claude/resize", `{"cols":120,"rows":50}`, http.StatusNoContent)
	if cols, rows := term.size(); cols != 120 || rows != 50 {
		t.Errorf("the terminal was resized to %d×%d, want 120×50", cols, rows)
	}
}

// A second stream for the same panel supersedes the first — a reconnect is the
// browser saying the old one is gone — and the terminal it replaces must be
// closed, or its process outlives every reload for the life of the daemon.
func TestReopeningAPanelClosesTheTerminalItReplaces(t *testing.T) {
	_, h, opens := termServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	first := openStream(t, srv.URL+"/api/term/crm/ssh/stream?cols=80&rows=24")
	defer first.Body.Close()
	second := openStream(t, srv.URL+"/api/term/crm/ssh/stream?cols=80&rows=24")
	defer second.Body.Close()

	got := opens.all()
	if len(got) != 2 {
		t.Fatalf("%d terminals opened, want 2", len(got))
	}
	if !got[0].term.isClosed() {
		t.Error("the superseded terminal is still open — its ssh process would outlive the reload")
	}
	if got[1].term.isClosed() {
		t.Error("the reconnect closed the terminal it just opened")
	}
}

// openStream opens an SSE stream and fails the test unless it is serving.
func openStream(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("%s = %d, want 200", url, resp.StatusCode)
	}
	return resp
}

// post sends a request the way the browser does — token cookie included — and
// checks the status.
func post(t *testing.T, url, body string, want int) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("POST %s = %d, want %d", url, resp.StatusCode, want)
	}
}

// readSSEUntil decodes the stream's data: events — each one base64 of raw
// terminal output — until marker turns up or the deadline passes.
func readSSEUntil(t *testing.T, body io.Reader, marker string, within time.Duration) (string, bool) {
	t.Helper()
	type result struct {
		out string
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		var seen bytes.Buffer
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			data, found := strings.CutPrefix(sc.Text(), "data: ")
			if !found {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				continue
			}
			seen.Write(raw)
			if strings.Contains(seen.String(), marker) {
				done <- result{seen.String(), true}
				return
			}
		}
		done <- result{seen.String(), false}
	}()

	select {
	case r := <-done:
		return r.out, r.ok
	case <-time.After(within):
		return "", false
	}
}
