package ui

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The local shell is the one terminal Forge opens without ssh, so the things that
// make it usable are the ones nothing else in the package checks: it must be a
// LOGIN shell (your profile, so your PATH and aliases), it must start in your home
// directory rather than wherever `forge ui` happened to be launched from, and it
// must tell the far end it is talking to a real terminal — the browser's xterm —
// or vim and htop draw into a teletype.
func TestLocalShellIsALoginShellInYourHome(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")

	tm, err := startLocalTerm(80, 24)
	if err != nil {
		t.Fatalf("start the local shell: %v", err)
	}
	defer tm.close()

	// argv[0] with a leading dash is the only portable way to say "login shell";
	// the -l flag is not one /bin/sh understands.
	if got := tm.cmd.Args[0]; got != "-sh" {
		t.Errorf("argv[0] = %q, want %q — the shell would not read your profile", got, "-sh")
	}
	if home, err := os.UserHomeDir(); err == nil && tm.cmd.Dir != home {
		t.Errorf("shell starts in %q, want your home %q", tm.cmd.Dir, home)
	}
	if !hasEnv(tm.cmd.Env, "TERM", "xterm-256color") {
		t.Error("TERM is not xterm-256color: full-screen programs would render for the daemon's terminal, not the browser's")
	}

	// And it has to actually BE a shell: run a command and read its output back.
	// The command is written as fo""rge-ok so the line the shell echoes back is not
	// itself the marker — only real output of a real shell can produce it.
	if _, err := tm.ptmx.Write([]byte("echo fo\"\"rge-ok\n")); err != nil {
		t.Fatalf("write to the shell: %v", err)
	}
	if out, ok := readUntil(tm, "forge-ok", 20*time.Second); !ok {
		t.Errorf("the shell never ran the command; read so far:\n%s", out)
	}
}

// hasEnv reports whether key resolves to want in a process environment, taking
// the LAST occurrence — which is the one exec keeps when a key appears twice
// (startLocalTerm appends TERM onto an environment that may already carry one).
func hasEnv(env []string, key, want string) bool {
	got, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			got, found = v, true
		}
	}
	return found && got == want
}

// readUntil reads the terminal until marker shows up or the deadline passes,
// returning everything it read (for the failure message) and whether it found it.
func readUntil(tm *term, marker string, within time.Duration) (string, bool) {
	out := make(chan []byte, 32)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tm.ptmx.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				out <- b
			}
			if err != nil {
				close(out)
				return
			}
		}
	}()

	deadline := time.After(within)
	var seen bytes.Buffer
	for {
		select {
		case b, ok := <-out:
			if !ok {
				return seen.String(), false // the shell exited without saying it
			}
			seen.Write(b)
			if strings.Contains(seen.String(), marker) {
				return seen.String(), true
			}
		case <-deadline:
			return seen.String(), false
		}
	}
}

// The whole path the browser uses, end to end: open the SSE stream (which starts
// the shell), type a command into it over the input endpoint, and read the output
// back out of the stream. Everything below this line is what the unit tests can't
// see — the registry key the stream and the input agree on, the base64 framing,
// and the fact that a shell started by an HTTP handler is a shell you can talk to.
func TestLocalShellStreamsOutputForACommandTypedIntoIt(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	_, h := testServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	client := &http.Client{}
	get, err := http.NewRequest("GET", srv.URL+"/api/term/local/stream?cols=80&rows=24", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	resp, err := client.Do(get)
	if err != nil {
		t.Fatalf("open the local shell stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}

	// Same marker trick as above: the shell's echo of the line you typed must not
	// be mistaken for the command having run.
	in := base64.StdEncoding.EncodeToString([]byte("echo fo\"\"rge-sse-ok\n"))
	post, err := http.NewRequest("POST", srv.URL+"/api/term/local/input", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	post.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	post.Header.Set("Content-Type", "text/plain")
	pr, err := client.Do(post)
	if err != nil {
		t.Fatalf("type into the local shell: %v", err)
	}
	pr.Body.Close()
	if pr.StatusCode != http.StatusNoContent {
		t.Fatalf("input = %d, want 204", pr.StatusCode)
	}

	got, ok := readSSEUntil(t, resp.Body, "forge-sse-ok", 20*time.Second)
	if !ok {
		t.Errorf("the command's output never reached the browser; stream carried:\n%s", got)
	}
}

// readSSEUntil decodes the stream's data: events — each one base64 of raw pty
// output — until marker turns up or the deadline passes.
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

// The local shell belongs to no workspace, so it must not be able to collide with
// one in the terminal registry — a workspace whose ssh shell replaced the local
// one (or the reverse) would close a shell you were typing in.
func TestLocalTerminalCannotCollideWithAWorkspaceTerminal(t *testing.T) {
	for _, ws := range []string{"local", "", "crm"} {
		for _, kind := range []string{termClaude, termSSH, termHost} {
			if termKey(ws, kind) == localTermKey {
				t.Errorf("workspace %q's %s terminal shares the local shell's registry key", ws, kind)
			}
		}
	}
}

// The local shell's endpoints take no workspace — there is one local machine, so
// asking which workspace you mean is meaningless. This is what proves they are
// wired up at all: without the routes these would be the mux's own 404, not the
// handler's answer that no such terminal is open.
func TestLocalTerminalHasWorkspaceLessRoutes(t *testing.T) {
	_, h := testServer(t)

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/term/local/input", base64.StdEncoding.EncodeToString([]byte("x"))},
		{"POST", "/api/term/local/resize", `{"cols":80,"rows":24}`},
	} {
		req := httptest.NewRequest(c.method, "http://127.0.0.1:47615"+c.path, strings.NewReader(c.body))
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (no local shell is open)", c.method, c.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "no terminal") {
			t.Errorf("%s %s answered %q — the route is not wired to the local terminal handler",
				c.method, c.path, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// One local machine, one local shell: every tab must resolve to the same session
// and the same ws-less endpoints. Keyed per workspace instead, each tab would
// silently start a shell of its own — five tabs, five shells, and the command you
// left running is in whichever one you opened it from.
func TestBrowserKeepsExactlyOneLocalShell(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	body := jsFunc(t, js, "shellKey")
	if !strings.Contains(body, `"local"`) {
		t.Error("shellKey does not special-case the local shell; it would be keyed per workspace")
	}
	if !strings.Contains(jsFunc(t, js, "termPath"), "/api/term/local/") {
		t.Error("termPath does not send the local shell to its workspace-less endpoints")
	}
	// Deleting a workspace, or losing one from the list, must not take the local
	// shell with it — it is not that workspace's, and killing it would drop
	// whatever you were running on your own machine.
	if !strings.Contains(jsFunc(t, js, "disposeWsShells"), "WS_SHELL_KINDS") {
		t.Error("disposeWsShells sweeps every kind; a deleted workspace would close the local shell")
	}
	if !strings.Contains(jsFunc(t, js, "pruneShells"), "isLocalKind(sess.kind)") {
		t.Error("pruneShells would dispose the local shell: it matches no workspace by name")
	}
}

// The rail button is the whole feature from a user's side, and the local shell is
// the one that keeps working when a server does not — so it must not be greyed out
// with the rest when the active workspace is unreachable.
func TestLocalShellButtonStaysUsableWhenTheWorkspaceIsNot(t *testing.T) {
	if html := embeddedAsset(t, "index.html"); !strings.Contains(html, `data-action="local"`) {
		t.Fatal("index.html has no local-shell rail button")
	}
	body := jsFunc(t, embeddedAsset(t, "app.js"), "renderStage")
	if !strings.Contains(body, `"local"`) {
		t.Error("renderStage disables the local-shell button along with the workspace's own actions")
	}
}
