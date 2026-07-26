package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A tunnel is the third thing the transport does, and the only one with no
// streams of its own: what proves it works is a connection made to a port on this
// machine coming out somewhere else. So these tests make one — a real service on
// one side, a real SSH server in the middle, a real dial on the other — because
// every part of a forward that can be wrong is invisible above a stub.

func TestTheGoClientCarriesALocalPortToTheFarEnd(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, noCommands)
	trust(t, srv)
	useGo(t)

	service := echoService(t, 0)
	local := freePort(t)

	tun, err := srv.target("crm").Forward(local, service)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	if got := roundTrip(t, "127.0.0.1", local, "ping"); got != "ping" {
		t.Errorf("the tunnel gave back %q, want what was written into it", got)
	}
	// Named, not resolved: the far end is whatever "localhost" means on the
	// server, which is what `-L local:localhost:remote` has always meant.
	want := "direct-tcpip localhost:" + strconv.Itoa(service)
	if got := srv.next(t, "a forwarded connection"); got != want {
		t.Errorf("the server was asked for %q, want %q", got, want)
	}
}

// "localhost" is two addresses on any machine with IPv6, and which one a program
// reaches for is not ours to predict — a browser opening the link the ports panel
// shows may well try ::1 first. OpenSSH binds both; so must this.
func TestATunnelAnswersOnBothLoopbackAddresses(t *testing.T) {
	if !hasIPv6Loopback() {
		t.Skip("no IPv6 loopback on this machine")
	}
	pub := writeClientKey(t)
	srv := startServer(t, pub, noCommands)
	trust(t, srv)
	useGo(t)

	service := echoService(t, 0)
	local := freePort(t)

	tun, err := srv.target("crm").Forward(local, service)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	if got := roundTrip(t, "127.0.0.1", local, "v4"); got != "v4" {
		t.Errorf("over IPv4 the tunnel gave back %q", got)
	}
	if got := roundTrip(t, "::1", local, "v6"); got != "v6" {
		t.Errorf("over IPv6 the tunnel gave back %q", got)
	}
}

// The local port being held by something else is not the server's fault and does
// not clear on its own, so the supervisor shows it as its own state and says what
// to kill. That only works if it can tell this failure from a network blip.
func TestATunnelOnAPortSomethingElseHoldsReportsItAsSuch(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, noCommands)
	trust(t, srv)
	useGo(t)

	held := echoService(t, 0) // this test is now the thing holding it

	_, err := srv.target("crm").Forward(held, 9000)
	if !errors.Is(err, ErrPortBusy) {
		t.Fatalf("err = %v, want ErrPortBusy", err)
	}

	// And the same when only the IPv6 half is taken: a tunnel that came up on
	// half of localhost works from some programs and not others, which is a worse
	// answer than saying the port is in use.
	if !hasIPv6Loopback() {
		return
	}
	free := freePort(t)
	v6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(free)))
	if err != nil {
		t.Skipf("could not hold the IPv6 half of port %d: %v", free, err)
	}
	defer v6.Close()

	if _, err := srv.target("crm").Forward(free, 9000); !errors.Is(err, ErrPortBusy) {
		t.Errorf("with ::1 held, err = %v, want ErrPortBusy", err)
	}
	// And nothing is left holding the half that was free.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(free)))
	if err != nil {
		t.Errorf("a forward that failed kept the IPv4 half of the port: %v", err)
	} else {
		ln.Close()
	}
}

// The supervisor stops retrying a key the server will never accept, and that
// decision is only as good as the transport's ability to name the failure — which
// x/crypto does not, so this backend has to.
func TestATunnelToAServerThatRefusesTheKeySaysSo(t *testing.T) {
	writeClientKey(t) // ours…
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := ssh.NewPublicKey(other) // …but the server wants another
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, accepted, noCommands)
	trust(t, srv)
	useGo(t)

	if _, err := srv.target("crm").Forward(freePort(t), 9000); !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

// Closing must free the port before it returns: the supervisor rebinds it a
// second later, and a tunnel that is still holding it makes its own retry look
// like somebody else's program.
func TestClosingATunnelEndsTheWaitAndReleasesThePort(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, noCommands)
	trust(t, srv)
	useGo(t)

	local := freePort(t)
	tun, err := srv.target("crm").Forward(local, 9000)
	if err != nil {
		t.Fatal(err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- tun.Wait() }()

	if err := tun.Close(); err != nil {
		t.Fatalf("close the tunnel: %v", err)
	}

	select {
	case err := <-stopped:
		// Closed on purpose is not a failure — the holder asked for it.
		if err != nil {
			t.Errorf("Wait after Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the tunnel was closed")
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		t.Fatalf("the port is still held after Close: %v", err)
	}
	ln.Close()

	// The front end of a tunnel has two owners as often as a terminal does.
	if err := tun.Close(); err != nil {
		t.Errorf("closing twice: %v", err)
	}
}

// A forward is lazy, and that is what makes it survivable: a workspace whose
// container is down must not cost its tunnel anything, or every restart of a
// service would need the supervisor to notice and rebuild.
func TestAFarEndThatIsNotListeningCostsTheTunnelNothing(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, noCommands)
	trust(t, srv)
	useGo(t)

	service := freePort(t) // nothing there yet
	local := freePort(t)

	tun, err := srv.target("crm").Forward(local, service)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	// The connection is accepted locally and then gets nothing, which is what a
	// caller would have got from a service that is not running.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		t.Fatalf("the tunnel refused a connection outright: %v", err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Errorf("read from a tunnel with nothing behind it: %v", err)
	}
	conn.Close()

	// And when the container comes back, so does the port — with nothing done to
	// the tunnel in between.
	echoService(t, service)
	if got := roundTrip(t, "127.0.0.1", local, "back"); got != "back" {
		t.Errorf("after the service came back the tunnel gave %q, want %q", got, "back")
	}
}

// A tunnel is only as alive as the connection under it, and the supervisor has to
// find out: a server that rebooted leaves a listener on this machine that accepts
// connections and carries them nowhere.
func TestATunnelStopsWhenItsConnectionDoes(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, noCommands)
	trust(t, srv)
	useGo(t)

	local := freePort(t)
	tun, err := srv.target("crm").Forward(local, 9000)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	stopped := make(chan error, 1)
	go func() { stopped <- tun.Wait() }()

	// The far end goes away, as a rebooting server's does.
	tun.(*goTunnel).client.Close()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the tunnel outlived its connection — it is accepting connections it cannot carry")
	}
	if _, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local))); err != nil {
		t.Errorf("a tunnel that stopped is still holding its port: %v", err)
	}
}

// The exec'd backend end to end, without a server to reach: a tunnel to nothing
// is started, exits, and comes back as the reason it exited rather than as a
// process's exit status. That path — Start, the goroutine, the buffered stderr,
// Wait — is the same one a working tunnel unwinds through.
func TestTheExecdBackendReportsWhySshGaveUp(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary")
	}
	prev := chosen
	Use(execBackend{})
	t.Cleanup(func() { Use(prev) })

	// Nothing is listening there, so ssh fails to connect and says so.
	dead := Target{User: "nobody", Addr: "127.0.0.1", Port: freePort(t)}
	tun, err := dead.Forward(freePort(t), 9000)
	if err != nil {
		t.Fatalf("starting ssh failed before it could run: %v", err)
	}
	defer tun.Close()

	stopped := make(chan error, 1)
	go func() { stopped <- tun.Wait() }()
	select {
	case err := <-stopped:
		if err == nil {
			t.Fatal("a tunnel that could not connect reported success")
		}
		// What ssh said, not the 255 it exits with for everything: the supervisor
		// puts this straight in front of the user, and "exit status 255" tells
		// nobody anything.
		if strings.Contains(err.Error(), "exit status") {
			t.Errorf("err = %q, want ssh's own words about why it gave up", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ssh never gave up on an address nothing is listening on")
	}
}

// The two failures worth naming, read off the prose each client produces. Both
// clients' wordings are here together because that is the point of doing it in
// the transport: the supervisor above switches on ErrAuth and ErrPortBusy and
// never learns either vocabulary.
func TestTheNamedFailuresAreRecognisedInBothClientsWording(t *testing.T) {
	auth := []string{
		"Permission denied (publickey).",
		"ssh: Too many authentication failures",
		"PUBLICKEY denied",
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]",
	}
	for _, s := range auth {
		if !authFailed(s) {
			t.Errorf("authFailed(%q) = false", s)
		}
	}
	busy := []string{
		"bind [127.0.0.1]:16104: Address already in use",
		"channel_setup_fwd_listener_tcpip: cannot listen to port: 16104",
	}
	for _, s := range busy {
		if !portBusy(s) {
			t.Errorf("portBusy(%q) = false", s)
		}
	}

	// The three categories must not bleed into each other: one is terminal, one
	// names a local program to kill, and everything else quietly retries.
	other := []string{
		"ssh: connect to host 1.2.3.4 port 22: Connection refused",
		"no route to host",
		"",
	}
	for _, s := range other {
		if authFailed(s) || portBusy(s) {
			t.Errorf("%q was read as one of the named failures", s)
		}
	}
	for _, s := range busy {
		if authFailed(s) {
			t.Errorf("a busy local port read as an auth failure: %q", s)
		}
	}
	for _, s := range auth {
		if portBusy(s) {
			t.Errorf("an auth failure read as a busy local port: %q", s)
		}
	}
}

// What the exec'd backend makes of the way ssh left, which is the whole of what
// that client can tell anyone: a first line worth repeating, and the two failures
// that mean something more.
func TestTheExecdTunnelReadsSshsLastWords(t *testing.T) {
	cases := []struct {
		stderr string
		is     error
		detail string
	}{
		{"Permission denied (publickey).", ErrAuth, "Permission denied (publickey)."},
		{"bind [127.0.0.1]:16104: Address already in use", ErrPortBusy, "Address already in use"},
		{
			"ssh: connect to host h port 22: Connection refused\r\nTry again later\n",
			nil,
			"Connection refused",
		},
	}
	for _, c := range cases {
		tun := &execTunnel{}
		tun.stderr.WriteString(c.stderr)

		err := tun.classify(errors.New("exit status 255"))
		if err == nil {
			t.Fatalf("classify(%q) = nil", c.stderr)
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("classify(%q) = %v, want %v", c.stderr, err, c.is)
		}
		// The first line only: ssh follows a failure with advice about known_hosts
		// and key permissions that says nothing about this tunnel.
		if !strings.Contains(err.Error(), c.detail) {
			t.Errorf("classify(%q) = %q, want it to carry %q", c.stderr, err, c.detail)
		}
		if strings.Contains(err.Error(), "Try again later") {
			t.Errorf("classify(%q) kept more than the first line: %q", c.stderr, err)
		}
	}

	// Nothing said and nothing wrong is nothing to report.
	if err := (&execTunnel{}).classify(nil); err != nil {
		t.Errorf("a tunnel that ended quietly reported %v", err)
	}
}

// noCommands is a server that runs nothing: these tests open no sessions, and a
// forward is not one.
func noCommands(string, io.Reader) (string, string, int) { return "", "", 0 }

// freePort is a port nothing is listening on. Racy in principle and not in
// practice — the alternative is asking the OS for a port and then handing the
// number to something that has to bind it itself.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoService is a stand-in for whatever a workspace publishes: it sends back
// what is sent to it, which is all it takes to show that bytes travel both ways.
// Port 0 lets the OS choose; a port given is the one it binds.
func echoService(t *testing.T, port int) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(conn, conn); conn.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// roundTrip writes one message into the local end of a tunnel and returns what
// comes back out of it.
func roundTrip(t *testing.T, host string, port int, msg string) string {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial the tunnel at %s:%d: %v", host, port, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("write into the tunnel: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read back from the tunnel: %v", err)
	}
	return string(buf)
}

func hasIPv6Loopback() bool {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
