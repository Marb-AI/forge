package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Reaching a server through another one, against real SSH servers on both ends
// — which is the only way to show the thing that matters here: that a jump host
// carries a stream and the handshake at the end of it belongs to the target.
//
// The test servers already answer direct-tcpip (the tunnels needed it), and they
// dial what the client asks for, so a jump in a test is a jump.

// TestTheGoClientReachesAServerThroughAJump is the shape of the whole feature: a
// command runs on the target, and the jump host was asked for nothing but a
// connection to it.
func TestTheGoClientReachesAServerThroughAJump(t *testing.T) {
	pub := writeClientKey(t)
	target := startServer(t, pub, func(cmd string, _ io.Reader) (string, string, int) {
		return "on the target: " + cmd, "", 0
	})
	jump := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "the jump ran a command, which it should never be asked to do", "", 0
	})
	trust(t, target)
	trust(t, jump)
	useGo(t)

	out, err := via(target, "crm", jump).Output("tmux", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "on the target: tmux ls" {
		t.Errorf("Output = %q — the command did not run on the target", out)
	}
	// What the jump was asked for: a stream to the target, and nothing else.
	if got, want := jump.next(t, "the forward"), "direct-tcpip "+target.addr.String(); got != want {
		t.Errorf("the jump was asked for %q, want %q", got, want)
	}
}

// The target's own key is what gets checked and recorded, because the handshake
// is with the target — a jump that could present its own key would be a jump
// that could read the session.
func TestAJumpDoesNotStandInForTheServerBehindIt(t *testing.T) {
	pub := writeClientKey(t)
	target := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	jump := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	dir := recordHostKeysIn(t)
	useGo(t)

	if _, err := via(target, "crm", jump).Output("id"); err != nil {
		t.Fatal(err)
	}

	// Both ends are first sights, and both are written down under their own
	// address with their own key.
	recorded := lines(t, filepath.Join(dir, "known_hosts"))
	for _, srv := range []*testServer{target, jump} {
		if !recordsKey(recorded, srv) {
			t.Errorf("no record of %s (%s) in %q", srv.addr, ssh.FingerprintSHA256(srv.hostKey), recorded)
		}
	}
}

// A terminal through a jump: the pty is the target's, resize reaches it, and the
// bytes go both ways. This is the path the UI's panels take, so it is the one
// that says "streaming still works".
func TestATerminalWorksThroughAJump(t *testing.T) {
	// The terminal type is this process's own, so it has to be this test's own
	// too — a machine with no TERM (every CI runner) sends the empty string, and
	// what is being checked here is the route, not the environment.
	t.Setenv("TERM", "xterm-256color")
	pub := writeClientKey(t)
	target := startTTYServer(t, pub, func(_ string, tty io.ReadWriter) {
		io.WriteString(tty, "claude> ")
		buf := make([]byte, 4)
		io.ReadFull(tty, buf)
		fmt.Fprintf(tty, "got %s", buf)
	})
	jump := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	trust(t, target)
	trust(t, jump)
	useGo(t)

	term, err := via(target, "crm", jump).Open(Shell{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if got := target.next(t, "the pty"); got != "pty xterm-256color 120x40" {
		t.Errorf("the target was asked for %q — the terminal is not the target's", got)
	}
	if got := target.next(t, "the shell"); got != "shell" {
		t.Errorf("the target was asked for %q, want the login shell", got)
	}
	if _, err := term.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := term.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, term, "got ping"); !strings.Contains(got, "claude> ") {
		t.Errorf("read %q, want the prompt and the echo", got)
	}
	if got := target.next(t, "the resize"); got != "window-change 100x30" {
		t.Errorf("the target was told %q about the window", got)
	}
}

// The route has no life of its own. Closing what the caller was given takes the
// jump connection with it — one command is one connection, still, and a UI
// daemon that opens hundreds does not leave a jump holding them all.
func TestClosingTheConnectionClosesTheJumpBehindIt(t *testing.T) {
	pub := writeClientKey(t)
	target := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	jump := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	trust(t, target)
	trust(t, jump)
	useGo(t)

	// Output opens a connection and closes it when the command is done, so by the
	// time it returns the jump should be on its way out too.
	if _, err := via(target, "crm", jump).Output("id"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-jump.gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the jump's connection is still up after the connection it carried ended")
	}
}

// A key the jump will not take is terminal in exactly the way one the target
// will not take is: the supervisor gives up on ErrAuth rather than retrying it
// forever, and it must not matter which end of the route refused.
func TestAJumpThatRefusesTheKeyIsAnAuthFailure(t *testing.T) {
	pub := writeClientKey(t)
	target := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := ssh.NewPublicKey(other)
	if err != nil {
		t.Fatal(err)
	}
	jump := startServer(t, wanted, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	trust(t, target)
	trust(t, jump)
	useGo(t)

	_, err = via(target, "crm", jump).Output("id")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want it named as an authentication failure", err)
	}
	// And it says which end refused, because "permission denied" against the
	// wrong server is an hour of looking in the wrong place.
	if !strings.Contains(err.Error(), "via ") || !strings.Contains(err.Error(), jump.addr.String()) {
		t.Errorf("the error does not name the jump: %v", err)
	}
}

// Hops are taken in the order they are written, each through the one before it —
// ssh's own reading of `-J a,b`.
func TestAChainOfJumpsIsTakenInOrder(t *testing.T) {
	pub := writeClientKey(t)
	target := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "arrived", "", 0
	})
	second := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	first := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	trust(t, target)
	trust(t, second)
	trust(t, first)
	useGo(t)

	out, err := via(target, "crm", first, second).Output("id")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "arrived" {
		t.Errorf("Output = %q, want the target's", out)
	}
	// The first hop is dialled from this machine and asked for the second; the
	// second is asked for the target. Nobody is asked for a server they are not
	// next to.
	if got, want := first.next(t, "the first forward"), "direct-tcpip "+second.addr.String(); got != want {
		t.Errorf("the first hop was asked for %q, want %q", got, want)
	}
	if got, want := second.next(t, "the second forward"), "direct-tcpip "+target.addr.String(); got != want {
		t.Errorf("the second hop was asked for %q, want %q", got, want)
	}
}

// An unreachable jump is bounded the same way an unreachable server is. Nothing
// in the library does this for a handshake over a stream, so it is done by hand
// — and a UI daemon whose every command waits out a TCP timeout is the failure
// this prevents.
func TestAJumpThatNeverAnswersIsGivenUpOn(t *testing.T) {
	pub := writeClientKey(t)
	target := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	// A listener that accepts and then says nothing: a handshake that starts and
	// never finishes, which no connect timeout on a TCP dial would catch.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	// Accepted and then left alone, deliberately — but each one is closed when the
	// test ends rather than deferred inside the loop, where the defers would pile
	// up unrun until the goroutine returned.
	over := make(chan struct{})
	defer close(over)
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return // listener closed: the test is over
			}
			go func() {
				<-over
				conn.Close()
			}()
		}
	}()
	trust(t, target)
	useGo(t)

	tgt := target.target("crm")
	tgt.Jump = "admin@" + silent.Addr().String()

	done := make(chan error, 1)
	go func() {
		_, err := tgt.Output("id")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a jump that never answered was treated as a connection")
		}
	case <-time.After((connectTimeout + 5) * time.Second):
		t.Fatalf("still waiting after %ds; the handshake through a jump is unbounded", connectTimeout+5)
	}
}

// via is the Target for reaching srv through the given servers, in order.
func via(srv *testServer, user string, jumps ...*testServer) Target {
	t := srv.target(user)
	var hops []string
	for _, j := range jumps {
		hops = append(hops, "admin@"+j.addr.String())
	}
	t.Jump = strings.Join(hops, ",")
	return t
}

// recordsKey reports whether one of the known_hosts lines is this server's, key
// and address both.
func recordsKey(recorded []string, srv *testServer) bool {
	host, port, _ := net.SplitHostPort(srv.addr.String())
	want := string(ssh.MarshalAuthorizedKey(srv.hostKey))
	for _, line := range recorded {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		named := fields[0] == host || fields[0] == "["+host+"]:"+port
		if named && strings.TrimSpace(want) == fields[1]+" "+fields[2] {
			return true
		}
	}
	return false
}

// readAll reads from term until want has arrived, so a test never depends on how
// the far end happened to split its writes. (gossh_test.go has readUntil, which
// answers "did this ever show up" with a bool; here the bytes themselves are
// what is being checked.)
func readAll(t *testing.T, r io.Reader, want string) string {
	t.Helper()
	var seen strings.Builder
	buf := make([]byte, 256)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, err := r.Read(buf)
		seen.Write(buf[:n])
		if strings.Contains(seen.String(), want) {
			return seen.String()
		}
		if err != nil {
			t.Fatalf("read %q before %v, waiting for %q", seen.String(), err, want)
		}
	}
	t.Fatalf("read %q, never saw %q", seen.String(), want)
	return ""
}
