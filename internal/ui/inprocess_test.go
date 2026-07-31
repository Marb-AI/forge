package ui

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// What a desktop shell gets when it starts the UI in its own process, and — just
// as much the point — what it does not: no pidfile, no token file, no signal
// handler, and therefore no argument with the `forge ui` daemon over any of them.

// wiredDeps is a complete, harmless wiring: every operation validate() insists on,
// none of them reaching a machine. Start needs a full one, because binding a port
// and refusing an incomplete wiring are the same step.
func wiredDeps() Deps {
	return Deps{
		ListWorkspaces:  func() ([]WorkspaceInfo, error) { return []WorkspaceInfo{}, nil },
		KnowsWorkspace:  func(string) bool { return false },
		OpenTerminal:    func(string, string, uint16, uint16) (Terminal, error) { return newFakeTerm(), nil },
		ListDir:         func(string, string) (DirListing, error) { return DirListing{}, nil },
		ReadFile:        func(string, string) (FileText, error) { return FileText{}, nil },
		Checkpoint:      func(string, io.Writer) error { return nil },
		StopSession:     func(string) error { return nil },
		RestartSession:  func(string) error { return nil },
		ListHosts:       func() ([]string, error) { return []string{}, nil },
		CreateWorkspace: func(string, string) error { return nil },
		PrepareHost:     func(string, string, bool, bool, bool, bool, bool, io.Writer) error { return nil },
		DeleteWorkspace: func(string) error { return nil },
		RemoveHost:      func(string) error { return nil },
		SetUIPort:       func(int) error { return nil },
		Ports:           func(string) (WorkspacePortsInfo, error) { return WorkspacePortsInfo{}, nil },
		ContainerAction: func(string, string, string) error { return nil },
	}
}

// started starts an instance on an OS-chosen port and stops it when the test ends.
func started(t *testing.T) *Instance {
	t.Helper()
	in, err := start(0, wiredDeps())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Stop() })
	return in
}

// browser is a client that behaves the way the webview will: it keeps the cookie
// the token bootstrap hands out and follows the redirect that strips the token
// from the address. Without a jar every request after the first is anonymous, so
// the whole guard would read as a refusal.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// get asks a running instance for a path over real TCP — no httptest, because
// what is under test is a server that bound a port of its own.
func get(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestStartServesOnThePortItReportsAndStops(t *testing.T) {
	in := started(t)

	if in.Port == 0 {
		t.Fatal("Start reported port 0; the caller has nothing to point a webview at")
	}
	if in.Token == "" {
		t.Fatal("Start reported no token; every request would be forbidden")
	}
	resp := get(t, browser(t), in.URL())
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the URL Start hands out answered %d, want the UI", resp.StatusCode)
	}

	if err := in.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	// Stopped means the port is given back, not just that requests are refused:
	// a shell that restarts its window has to be able to bind again.
	if _, err := http.Get(in.URL()); err == nil {
		t.Error("the UI still answers after Stop")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(in.Port)))
	if err != nil {
		t.Errorf("port %d is still held after Stop: %v", in.Port, err)
	} else {
		_ = ln.Close()
	}
}

// The guard is not the daemon's; it is the server's. An in-process UI is on the
// same loopback as everything else on the machine, so the token still decides.
func TestStartIsGuardedByItsToken(t *testing.T) {
	in := started(t)

	anon := browser(t)
	resp := get(t, anon, "http://127.0.0.1:"+strconv.Itoa(in.Port)+"/api/workspaces")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an in-process UI answered %d without a token, want 403", resp.StatusCode)
	}
	resp = get(t, anon, URL(in.Port, "wrong-token"))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong token answered %d, want 403", resp.StatusCode)
	}
}

// URL and the guard have to be each other's inverse: whatever the token is, the
// address handed to a webview must arrive at the server as that same token.
// Today's is hex and would survive either way — this is about the day it isn't.
func TestURLCarriesAnyTokenIntact(t *testing.T) {
	for _, token := range []string{
		"0f1e2d3c4b5a69788796a5b4c3d2e1f0", // what newToken mints today
		"a+b/c=d&e f?g#h%i",                // and what a wider alphabet would look like
	} {
		u, err := url.Parse(URL(4747, token))
		if err != nil {
			t.Fatalf("URL(%q) is not a URL: %v", token, err)
		}
		if got := u.Query().Get("t"); got != token {
			t.Errorf("token %q arrives as %q — that URL opens a UI which refuses it", token, got)
		}
	}
	if q := mustParse(t, URL(4747, "")).RawQuery; q != "" {
		t.Errorf("no token should mean no query, got %q", q)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Two at once is the case that matters: a desktop app started this way runs on a
// machine where `forge ui` may also be up. Nothing is shared for them to fight
// over — not the port, not the token, not a file.
func TestTwoInstancesShareNothing(t *testing.T) {
	a, b := started(t), started(t)

	if a.Port == b.Port {
		t.Fatalf("both instances took port %d", a.Port)
	}
	if a.Token == b.Token {
		t.Fatal("both instances minted the same token")
	}
	if resp := get(t, browser(t), URL(b.Port, a.Token)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("one instance's token was accepted by the other (%d)", resp.StatusCode)
	}
	// Stopping one leaves the other serving.
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if resp := get(t, browser(t), b.URL()); resp.StatusCode != http.StatusOK {
		t.Error("stopping one instance took the other down")
	}
}

// Start writes nothing into the state directory. The pidfile and the token file
// are how `forge ui` finds and stops the daemon; an in-process UI that wrote them
// would be telling the CLI to kill the desktop app.
func TestStartLeavesTheDaemonsFilesAlone(t *testing.T) {
	dir := t.TempDir()
	in, err := Start(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()

	for _, name := range []string{"ui.pid", "ui.token"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Start wrote %s; that file belongs to the `forge ui` daemon", name)
		}
	}
}

// Stopping twice is the normal case, not a bug: a shell that closes its window
// and then quits calls it on both paths.
func TestStopIsIdempotent(t *testing.T) {
	in := started(t)
	if err := in.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := in.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// The terminals an instance opened are its own, and they end with it. Nothing
// else will close them: they are processes on a server, and the shell that called
// Stop is about to exit.
func TestStopClosesTheTerminals(t *testing.T) {
	in := started(t)
	term := newFakeTerm()
	in.s.terms.replace(termKey("ws", termSSH), term)

	if err := in.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !term.isClosed() {
		t.Error("Stop left a terminal open")
	}
}

// The daemon path is unchanged, and this is the invariant it rests on: the
// pidfile means "bound and serving". Serve that could not take the port must
// leave no pidfile behind, or `forge ui` would wait for one that names nothing
// and then open a browser at a dead address.
func TestServeClaimsNothingIfItCannotBind(t *testing.T) {
	dir := t.TempDir()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	if err := Serve(dir, port, wiredDeps()); err == nil {
		t.Fatal("Serve on a taken port returned no error")
	}
	for _, name := range []string{"ui.pid", "ui.token"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Serve wrote %s though it never bound", name)
		}
	}
}

// And the daemon still does write both, in the order `forge ui` depends on: the
// token first, the pidfile only once it is serving. This is the whole of what
// Start does differently, so it is worth pinning down that Serve still does it.
func TestServeWritesTheDaemonsFilesAndStopsOnASignal(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)

	done := make(chan error, 1)
	go func() { done <- Serve(dir, port, wiredDeps()) }()

	// Wait until it answers. The signal handler is installed before serving
	// begins, so a reply is proof that a SIGTERM below will be caught rather
	// than kill this test binary.
	token := awaitServing(t, dir, port)

	pid, err := os.ReadFile(filepath.Join(dir, "ui.pid"))
	if err != nil {
		t.Fatalf("the daemon serves but wrote no pidfile: %v", err)
	}
	if string(pid) != strconv.Itoa(os.Getpid()) {
		t.Errorf("pidfile says %q, want this process (%d)", pid, os.Getpid())
	}
	if resp := get(t, browser(t), URL(port, token)); resp.StatusCode != http.StatusOK {
		t.Error("the token on disk is not the one being served")
	}

	// Signalled through os.Process rather than syscall.Kill, which does not exist
	// on Windows — this package builds there, and a test file that does not is a
	// build failure for the whole package rather than a skipped test.
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after SIGTERM")
	}
	if _, err := os.Stat(filepath.Join(dir, "ui.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the daemon left its pidfile behind; the next start would think it was running")
	}
}

// awaitServing waits for the daemon to be up and returns the token it wrote.
func awaitServing(t *testing.T, dir string, port int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		token, err := os.ReadFile(filepath.Join(dir, "ui.token"))
		if err == nil {
			if resp, err := http.Get(URL(port, string(token))); err == nil {
				resp.Body.Close()
				return string(token)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the daemon never came up")
	return ""
}

// freePort returns a port nothing is listening on. Racy in principle, which is
// why the daemon under test is the one that binds it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
