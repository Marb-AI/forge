package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// testServer is a one-connection-at-a-time SSH server on localhost.
type testServer struct {
	addr    net.Addr
	hostKey ssh.PublicKey
	// commands records every command line the server was asked to run.
	commands chan string
}

// startServer brings up a server that accepts clientKey and answers every exec
// request with run. It returns once it is listening.
func startServer(t *testing.T, clientKey ssh.PublicKey, run remoteRun) *testServer {
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

	srv := &testServer{addr: ln.Addr(), hostKey: signer.PublicKey(), commands: make(chan string, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed: the test is over
			}
			go srv.serve(conn, cfg, run)
		}
	}()
	return srv
}

func (s *testServer) serve(conn net.Conn, cfg *ssh.ServerConfig, run remoteRun) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sshConn.Close()
	// Keepalives arrive here; the library answers what it can and the rest are
	// declined, which is the reply a keepalive is looking for either way.
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only sessions here")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go s.session(ch, chReqs, run)
	}
}

func (s *testServer) session(ch ssh.Channel, reqs <-chan *ssh.Request, run remoteRun) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			return
		}
		req.Reply(true, nil)
		select {
		case s.commands <- payload.Command:
		default:
		}

		stdout, stderr, exit := run(payload.Command, ch)
		io.WriteString(ch, stdout)
		io.WriteString(ch.Stderr(), stderr)
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exit)}))
		return
	}
}

// target is the Target that reaches this server.
func (s *testServer) target(user string) Target {
	host, port, _ := net.SplitHostPort(s.addr.String())
	p, _ := strconv.Atoi(port)
	return Target{User: user, Addr: host, Port: p}
}

// writeClientKey generates this test's identity and puts it where the backend
// looks for one, returning its public half for the server to accept.
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
	dir := sshDirForTest(t)
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// trust records the server's host key in ~/.ssh/known_hosts, which is the only
// way this backend will talk to it.
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
func knownHostsPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sshDirForTest(t), "known_hosts")
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
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
	if got := <-srv.commands; got != "tmux ls" {
		t.Errorf("the server was asked to run %q, want %q", got, "tmux ls")
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

// A server nobody has vouched for is refused, and the error says how to vouch
// for it — because this backend cannot yet do that itself, and "handshake
// failed: knownhosts: key is unknown" tells a user nothing they can act on.
func TestTheGoClientRefusesAServerThatIsNotInKnownHosts(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	knownHostsPath(t) // the file exists; this server is deliberately not in it
	useGo(t)

	_, err := srv.target("crm").Output("id")
	if err == nil {
		t.Fatal("an unknown server was accepted")
	}
	if !strings.Contains(err.Error(), "not in") || !strings.Contains(err.Error(), backendEnv) {
		t.Errorf("error does not say what to do about it: %v", err)
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

// The agent is consulted on every connection, and there is a connection per
// command — so a socket left open is a file descriptor per command, and a daemon
// that polls every workspace runs out of them. It has to be released when the
// handshake that needed it is over.
func TestTheAgentSocketIsReleasedAfterEveryCommand(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "ok", "", 0
	})
	trust(t, srv)
	useGo(t)
	agentConns := startStubAgent(t)

	if _, err := srv.target("crm").Output("id"); err != nil {
		t.Fatal(err)
	}

	conn := <-agentConns
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the agent socket is still open after the command: read gave %v, want EOF", err)
	}
}

// An agent that is running but holds nothing useful must not stop the key files
// from being tried — x/crypto walks authentication methods by name and never
// goes back to one it has tried, so offering the agent separately would let a
// colleague's empty agent lock Forge out of every server it can reach today.
func TestAnAgentWithNoKeysDoesNotShadowTheKeyFiles(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "reached", "", 0
	})
	trust(t, srv)
	useGo(t)
	startStubAgent(t)

	out, err := srv.target("crm").Output("id")
	if err != nil {
		t.Fatalf("an empty agent shadowed ~/.ssh: %v", err)
	}
	if string(out) != "reached" {
		t.Errorf("Output = %q, want the server's stdout", out)
	}
}

// startStubAgent listens on a unix socket, points SSH_AUTH_SOCK at it, and hands
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
	prev := chosen
	Use(goBackend{})
	t.Cleanup(func() { Use(prev) })
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
