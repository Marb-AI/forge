package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// The pure-Go backend is tested against a real SSH server — one built out of the
// same library, listening on localhost for the length of a test.
//
// A fake would prove nothing here. Everything this backend has to get right is
// protocol: that the host key is checked against known_hosts, that the key in
// ~/.ssh is offered and accepted, that the command arrives as one string for the
// login shell, that stdin reaches it and its two output streams come back apart,
// and that a non-zero exit is a status on the channel rather than an error on
// the connection. None of that is visible above a stub.

// remoteRun is a test server's stand-in for a login shell: it is handed the
// command line as the client sent it, and answers with output and an exit code.
type remoteRun func(cmd string, stdin io.Reader) (stdout, stderr string, exit int)

// remoteTTY is the same stand-in for a session that got a terminal: it is handed
// the channel itself and stays on it, because a terminal has no output to collect
// and no end the client is waiting for.
type remoteTTY func(cmd string, tty io.ReadWriter)

// testServer is a one-connection-at-a-time SSH server on localhost.
type testServer struct {
	addr    net.Addr
	hostKey ssh.PublicKey
	// events records what the server was asked for, in the order it was asked:
	// "pty <term> <cols>x<rows>", "window-change <cols>x<rows>", "agent-forward",
	// "shell", "exec <command line>". Order is half of what the terminal tests
	// check — a tmux attach that arrives before its pty is an attach that fails.
	events chan string
	// run answers exec requests, tty answers a session that asked for a terminal.
	run remoteRun
	tty remoteTTY
	// gone closes when a client's connection ends, however it ends. (Once: a test
	// may open a second connection to the same server, and the first one to end is
	// the one being waited for.)
	gone     chan struct{}
	goneOnce sync.Once
}

// startServer brings up a server that accepts clientKey and answers every exec
// request with run. It returns once it is listening.
func startServer(t *testing.T, clientKey ssh.PublicKey, run remoteRun) *testServer {
	t.Helper()
	return start(t, clientKey, run, nil)
}

// startTTYServer brings up one that hands every session a terminal and leaves tty
// on the far end of it.
func startTTYServer(t *testing.T, clientKey ssh.PublicKey, tty remoteTTY) *testServer {
	t.Helper()
	return start(t, clientKey, nil, tty)
}

func start(t *testing.T, clientKey ssh.PublicKey, run remoteRun, tty remoteTTY) *testServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientKey.Marshal()) {
				return nil, errors.New("unknown key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srv := &testServer{
		addr:    ln.Addr(),
		hostKey: signer.PublicKey(),
		events:  make(chan string, 16),
		run:     run,
		tty:     tty,
		gone:    make(chan struct{}),
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed: the test is over
			}
			go srv.serve(conn, cfg)
		}
	}()
	return srv
}

func (s *testServer) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer s.goneOnce.Do(func() { close(s.gone) })
	defer sshConn.Close()
	// Keepalives arrive here; the library answers what it can and the rest are
	// declined, which is the reply a keepalive is looking for either way.
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				return
			}
			go s.session(ch, chReqs)
		case "direct-tcpip":
			go s.direct(newCh)
		default:
			newCh.Reject(ssh.UnknownChannelType, "not something this server does")
		}
	}
}

// direct answers the channel a local forward opens: it connects to the address
// the client named — resolving it here, on the server's side, which is the whole
// meaning of `-L port:localhost:port` — and joins the two ends.
func (s *testServer) direct(newCh ssh.NewChannel) {
	var payload struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
		newCh.Reject(ssh.ConnectionFailed, "unreadable direct-tcpip request")
		return
	}
	s.record("direct-tcpip %s:%d", payload.Host, payload.Port)

	conn, err := net.Dial("tcp", net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))))
	if err != nil {
		// What a real server says when nothing is listening there, and what makes a
		// forward lazy: this connection fails, the tunnel does not.
		newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(ch, conn); ch.Close() }()
	go func() { io.Copy(conn, ch); conn.Close() }()
}

// record notes an event, dropping it rather than blocking if a test is not
// reading — a server that stalls because nobody drained it is a hang, not a
// failure anybody can read.
func (s *testServer) record(format string, args ...any) {
	select {
	case s.events <- fmt.Sprintf(format, args...):
	default:
	}
}

func (s *testServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// term, then the size in characters, then in pixels, then the modes.
			var payload struct {
				Term                          string
				Cols, Rows, WidthPx, HeightPx uint32
				Modes                         string
			}
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				return
			}
			s.record("pty %s %dx%d", payload.Term, payload.Cols, payload.Rows)
			req.Reply(true, nil)

		case "window-change":
			var payload struct{ Cols, Rows, WidthPx, HeightPx uint32 }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				return
			}
			s.record("window-change %dx%d", payload.Cols, payload.Rows)
			// No reply: a window change is told, not asked.

		case "auth-agent-req@openssh.com":
			s.record("agent-forward")
			req.Reply(true, nil)

		case "shell":
			s.record("shell")
			req.Reply(true, nil)
			// On its own goroutine, and the request loop keeps running: a terminal
			// stays open, and the requests that matter most arrive *while* it is
			// open — a window change during a session nobody is answering is a
			// resize that never happens.
			go s.tty("", ch)

		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				return
			}
			s.record("exec %s", payload.Command)
			req.Reply(true, nil)
			if s.tty != nil {
				go s.tty(payload.Command, ch)
				continue
			}
			stdout, stderr, exit := s.run(payload.Command, ch)
			io.WriteString(ch, stdout)
			io.WriteString(ch.Stderr(), stderr)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exit)}))
			return

		default:
			req.Reply(false, nil)
		}
	}
}

// next is the server's next event, or a failure saying which one never came.
//
// Waiting on the channel bare is what a request the server does not answer looks
// like from here, and it looks like nothing at all: the test hangs until the whole
// package times out, ten minutes later, and the report is a goroutine dump rather
// than a sentence. A request that goes unanswered is exactly the bug these tests
// exist to catch, so it has to arrive as one.
func (s *testServer) next(t *testing.T, want string) string {
	t.Helper()
	select {
	case ev := <-s.events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatalf("the server was never asked for %s — the request did not arrive, or it "+
			"arrived while the session was too busy to read it", want)
		return ""
	}
}

// target is the Target that reaches this server.
func (s *testServer) target(user string) Target {
	host, port, _ := net.SplitHostPort(s.addr.String())
	p, _ := strconv.Atoi(port)
	return Target{User: user, Addr: host, Port: p}
}

// writeClientKey generates this test's identity and hands it to the transport,
// returning its public half for the server to accept.
//
// Handed in rather than written to ~/.ssh, which is where it used to go and
// where nothing looks any more: the device key is given to the transport by
// whoever wired it, and in a test that is the test.
func writeClientKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	useIdentity(t, pem.EncodeToMemory(block))

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// useIdentity points the transport at some key material for one test and puts
// back whatever was there afterwards — the seam is process-wide, like the chosen
// backend and the known-hosts directory.
func useIdentity(t *testing.T, key []byte) {
	t.Helper()
	useIdentityFn(t, func() ([]byte, error) { return key, nil })
}

// useIdentityFn is the same for a source that answers something other than key
// material — the three ways there can be no key to offer.
//
// The old value is read under the lock that guards it, not beside it. Nothing
// writes it from another goroutine today, so this is not a race being fixed; it
// is one not being left for whoever adds a parallel test or a dial that outlives
// the test that started it.
func useIdentityFn(t *testing.T, key func() ([]byte, error)) {
	t.Helper()
	identityMu.Lock()
	prev := identityFn
	identityMu.Unlock()

	IdentityFrom(key)
	t.Cleanup(func() { IdentityFrom(prev) })
}

// trust records the server's host key in ~/.ssh/known_hosts, where this backend
// reads what the ssh binary already knows. Recording it there rather than in
// Forge's own file is deliberate for the tests that only need a server they are
// allowed to talk to: it leaves first sight — and therefore what gets written —
// to the tests about trust (see knownhosts_test.go).
func trust(t *testing.T, srv *testServer) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(srv.addr.String())}, srv.hostKey)
	f, err := os.OpenFile(knownHostsPath(t), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// knownHostsPath is ~/.ssh/known_hosts, created empty if it is not there — an
// absent file is a different failure ("nothing can be verified") from a host
// that is simply not in it, and a test that wants the second must not get the
// first by accident.
//
// Emptied after each test, and that is not tidiness. HOME is one directory for
// the whole package (see TestMain), the servers here listen on a port the
// operating system picks, and this file records a host BY that port — so a port
// handed out twice in one run means a later server inheriting an earlier one's
// key. What the client then reports is "the host key has CHANGED", which is
// correct, and nothing to do with the test asking.
func knownHostsPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sshDirForTest(t), "known_hosts")
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Reported rather than swallowed: an emptying that quietly did not happen
	// leaves the next test reading this one's hosts, which is the failure this
	// cleanup exists to prevent — and it would come back as somebody else's
	// mysterious "the host key has CHANGED".
	t.Cleanup(func() {
		if err := os.Truncate(path, 0); err != nil {
			t.Errorf("could not empty %s, so the next test inherits these hosts: %v", path, err)
		}
	})
	return path
}

func TestTheGoClientRunsACommandAndBringsBackWhatItPrinted(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(cmd string, _ io.Reader) (string, string, int) {
		return "claude\nforge\n", "", 0
	})
	trust(t, srv)
	useGo(t)

	out, err := srv.target("crm").Output("tmux", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "claude\nforge\n" {
		t.Errorf("Output = %q, want the server's stdout", out)
	}
	// One string for the login shell to parse, exactly as `ssh host tmux ls`
	// would have sent it.
	if got := srv.next(t, "the command"); got != "exec tmux ls" {
		t.Errorf("the server was asked for %q, want %q", got, "exec tmux ls")
	}
}

// Piping a provisioning script to a host is how `forge host prepare` works, and
// it is the one call that both writes and reads at length.
func TestTheGoClientPipesStdinAndKeepsTheOutputStreamsApart(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(_ string, stdin io.Reader) (string, string, int) {
		script, _ := io.ReadAll(stdin)
		return "ran " + string(script), "a warning", 0
	})
	trust(t, srv)
	useGo(t)

	var out, errOut strings.Builder
	err := srv.target("root").Pipe(strings.NewReader("set -e"), &out, &errOut, "bash -s")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ran set -e" {
		t.Errorf("stdout = %q — the script did not arrive on stdin", out.String())
	}
	if errOut.String() != "a warning" {
		t.Errorf("stderr = %q, want it on its own stream", errOut.String())
	}
}

// The file browser reads these codes to tell "no such path" from "not a
// directory", so they have to survive the change of client intact.
func TestTheGoClientReportsARemoteExitCode(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 5
	})
	trust(t, srv)
	useGo(t)

	_, err := srv.target("crm").Output("test", "-e", "gone")

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if exit.ExitCode() != 5 {
		t.Errorf("ExitCode() = %d, want 5", exit.ExitCode())
	}
}

// The key-only stance is the same one the exec'd backend spends two options on:
// a key the server will not have must fail immediately rather than fall back to
// asking for a password nobody is there to type.
func TestTheGoClientOffersOnlyKeys(t *testing.T) {
	writeClientKey(t) // ours…
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := ssh.NewPublicKey(other) // …but the server wants another
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, accepted, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	trust(t, srv)
	useGo(t)

	done := make(chan error, 1)
	go func() {
		_, err := srv.target("crm").Output("id")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refused key was reported as success")
		}
		if !strings.Contains(err.Error(), "unable to authenticate") {
			t.Errorf("unexpected failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client is waiting on something — a prompt, most likely")
	}
}

// A terminal from this backend has its pty on the *server*. The request that
// asks for one, the window change that resizes it and the channel that carries
// the bytes are the whole of it — no process on this machine, no pty on this
// machine, and so nothing a phone has to be missing.
func TestTheGoClientOpensATerminalOnTheServer(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	pub := writeClientKey(t)
	srv := startTTYServer(t, pub, echoTTY)
	trust(t, srv)
	useGo(t)

	term, err := srv.target("crm").Open(Shell{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	// The pty comes first, at the size the caller asked for and with this
	// process's own terminal type — the same two things the exec'd ssh sends from
	// the pty it was given.
	if got := srv.next(t, "a pty"); got != "pty xterm-256color 100x30" {
		t.Errorf("first request = %q, want the pty at the size asked for", got)
	}
	// And then a login shell, because no command was named: what `ssh host` alone
	// gives you.
	if got := srv.next(t, "a login shell"); got != "shell" {
		t.Errorf("second request = %q, want a login shell", got)
	}

	if _, err := term.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write to the terminal: %v", err)
	}
	if seen, ok := readUntil(term, "hello", 10*time.Second); !ok {
		t.Errorf("nothing came back from the terminal; read so far: %q", seen)
	}
}

// The Claude terminal is a command on a terminal, not a shell: the attach has to
// arrive *after* the pty, or tmux refuses it ("open terminal failed: not a
// terminal") and the panel opens onto an error.
func TestATerminalRunsItsCommandOnThePtyItAskedFor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	pub := writeClientKey(t)
	srv := startTTYServer(t, pub, echoTTY)
	trust(t, srv)
	useGo(t)

	// No size given, so it opens at the classic default — the size a pty has when
	// nobody says otherwise, which is what the exec'd backend's pty would be.
	term, err := srv.target("crm").Open(Shell{Remote: []string{"tmux", "attach"}})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if got := srv.next(t, "a pty"); got != "pty xterm-256color 80x24" {
		t.Errorf("first request = %q, want a pty at 80x24", got)
	}
	if got := srv.next(t, "the attach"); got != "exec tmux attach" {
		t.Errorf("second request = %q, want the attach as one command line", got)
	}
}

// Resizing is the reason a Terminal has a third method: the browser's window is
// the real one, and the program drawing into it only finds out if the server is
// told.
func TestResizingATerminalTellsTheServerTheWindowChanged(t *testing.T) {
	pub := writeClientKey(t)
	srv := startTTYServer(t, pub, echoTTY)
	trust(t, srv)
	useGo(t)

	term, err := srv.target("crm").Open(Shell{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	srv.next(t, "a pty")
	srv.next(t, "a login shell")

	if err := term.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if got := srv.next(t, "a window change"); got != "window-change 120x40" {
		t.Errorf("after Resize the server saw %q, want the new size", got)
	}
}

// Agent forwarding is what makes git inside a workspace shell use your keys with
// nothing stored on the server, and it is the one thing a terminal kind asks for
// beyond its command — so it has to be requested when asked for, and not
// otherwise: the host shell deliberately does without your git keys.
func TestATerminalForwardsTheAgentOnlyWhenAskedTo(t *testing.T) {
	pub := writeClientKey(t)
	srv := startTTYServer(t, pub, echoTTY)
	trust(t, srv)
	useGo(t)
	startStubAgent(t)

	asked, err := srv.target("crm").Open(Shell{ForwardAgent: true, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer asked.Close()
	if got := srv.next(t, "the agent"); got != "agent-forward" {
		t.Errorf("first request = %q, want the agent offered before the session starts", got)
	}
	srv.next(t, "a pty")
	srv.next(t, "a login shell")

	plain, err := srv.target("admin").Open(Shell{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if got := srv.next(t, "a pty"); strings.HasPrefix(got, "agent-forward") {
		t.Error("a terminal that did not ask for the agent forwarded it anyway")
	}
}

// Closing a terminal is closing the connection under it, which is exactly what
// killing the ssh process was: the remote shell goes with it, and a tmux client
// goes while the session it was attached to stays. The front end holding it may
// close it twice — a stream that ended and a panel that was replaced are two
// owners of one object — so twice must be safe.
func TestClosingATerminalTakesTheConnectionWithIt(t *testing.T) {
	pub := writeClientKey(t)
	srv := startTTYServer(t, pub, echoTTY)
	trust(t, srv)
	useGo(t)

	term, err := srv.target("crm").Open(Shell{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("close the terminal: %v", err)
	}

	select {
	case <-srv.gone:
	case <-time.After(10 * time.Second):
		t.Error("the connection outlived the terminal — the remote shell is still running")
	}
	if seen, ok := readUntil(term, "anything", time.Second); ok {
		t.Errorf("a closed terminal is still producing output: %q", seen)
	}
	if err := term.Close(); err != nil {
		t.Errorf("closing twice: %v", err)
	}
}

// echoTTY is a terminal with echo on and nothing else behind it: whatever is
// typed comes straight back, which is all it takes to show that bytes travel
// both ways over the channel the pty is attached to.
func echoTTY(_ string, tty io.ReadWriter) { io.Copy(tty, tty) }

// readUntil reads until marker shows up or the deadline passes, returning
// everything it read (for the failure message) and whether it found it.
func readUntil(r io.Reader, marker string, within time.Duration) (string, bool) {
	found := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), marker) {
				found <- seen.String()
				return
			}
			if err != nil {
				close(found)
				return
			}
		}
	}()
	select {
	case seen, ok := <-found:
		return seen, ok
	case <-time.After(within):
		return "", false
	}
}

// startStubAgent listens on a unix socket, points SSH_AUTH_SOCK at it, and hands
//
// It is here for agent FORWARDING, which is a different thing from authenticating
// with an agent: the device key is what gets this client in, and what a terminal
// lends to the far end is your agent, for git inside the workspace. Nothing in
// this package reads it to log in any more.
// back every connection it accepts.
//
// It answers "I have no keys", which is a real answer rather than a hang: the
// client asks for identities before it gives up on the agent and moves on to the
// key files, and a stub that never replies would leave the handshake waiting.
func startStubAgent(t *testing.T) <-chan net.Conn {
	t.Helper()
	dir, err := os.MkdirTemp("", "sshx-agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("SSH_AUTH_SOCK", sock)

	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// One request in (SSH2_AGENTC_REQUEST_IDENTITIES), one answer out:
				// message 12, zero keys. Then the connection is the client's to close,
				// which is the whole point of the test.
				var head [4]byte
				if _, err := io.ReadFull(conn, head[:]); err != nil {
					return
				}
				n := int(head[0])<<24 | int(head[1])<<16 | int(head[2])<<8 | int(head[3])
				if _, err := io.ReadFull(conn, make([]byte, n)); err != nil {
					return
				}
				conn.Write([]byte{0, 0, 0, 5, 12, 0, 0, 0, 0})
			}()
			conns <- conn
		}
	}()
	return conns
}

// useGo selects the pure-Go backend for one test.
func useGo(t *testing.T) {
	t.Helper()
	useBackend(t, goBackend{})
}

// sshDirForTest is ~/.ssh under the package's throwaway HOME, created on demand.
func sshDirForTest(t *testing.T) string {
	t.Helper()
	dir, err := sshDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
