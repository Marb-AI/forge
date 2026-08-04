package sshx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// goBackend is Forge's own SSH client: golang.org/x/crypto/ssh, no subprocess.
//
// It exists for the platforms where the exec'd backend cannot: a phone has no
// ssh binary and no way to spawn one, so either the client is library code or
// Forge is a laptop program forever. It is what runs now, unless somebody asks
// for the other one (FORGE_SSH_BACKEND=exec).
//
// Where it matches the exec'd backend it does so on purpose: the same connect
// timeout, the same keepalives, the same key-only stance, the same command string
// on the wire, and — for a terminal — the same terminal type and window size,
// asked of the server instead of taken from a pty on this machine. Host keys
// are its own now: it trusts a server on first sight and writes it down itself,
// as the exec'd backend's accept-new does, in a file Forge owns — see
// knownhosts.go.
//
// It authenticates with this device's own key, handed in by the core (see
// identity.go). Nothing here reads ~/.ssh, consults an agent, or wants a $HOME
// — which is the whole point, because a phone has none of the three.
type goBackend struct{}

func (goBackend) Name() string { return "go" }

func (b goBackend) Run(t Target, c Command) error {
	line := c.line()
	if line == "" {
		// An empty command means "give me a login shell" — an interactive
		// session, which this seam does not carry. Better to say so than to open
		// a shell nobody is attached to and wait for it to end.
		return fmt.Errorf("ssh %s: no command given (this transport runs commands, not shells)", t.dest())
	}

	client, err := dial(t)
	if err != nil {
		return err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh %s: %w", t.dest(), err)
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = c.Stdin, c.Stdout, c.Stderr

	// Run, not Start+Wait: the caller is waiting for the command to finish, and
	// the streams are drained by the session for as long as it runs.
	err = sess.Run(line)

	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return &ExitError{Code: exit.ExitStatus(), Err: err}
	}
	return err
}

// Open asks the server for a pty and starts a session on it.
//
// This is the terminal the exec'd backend gets by putting a pty in front of ssh
// on this machine, obtained the way the protocol has always offered it instead:
// a pty-req on the channel, a window-change when the browser is resized, and the
// channel itself carrying the bytes. There is no process, no local pty and no
// argv in it, which is the only shape a terminal can have on a phone.
//
// The connection belongs to the terminal and closes with it: one terminal is one
// connection, as one terminal was one ssh process before.
func (b goBackend) Open(t Target, s Shell) (Terminal, error) {
	client, err := dial(t)
	if err != nil {
		return nil, err
	}
	term, err := openTerm(client, t, s)
	if err != nil {
		client.Close()
		return nil, err
	}
	return term, nil
}

// openTerm sets up the session on an already-dialled client. Everything it can
// fail at happens before the terminal exists, so a failed open leaves nothing
// running and nothing to close but the connection.
func openTerm(client *ssh.Client, t Target, s Shell) (*remoteTerm, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}

	cols, rows := s.size()
	if err := sess.RequestPty(termType(), int(rows), int(cols), ptyModes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh %s: the server would not give this session a terminal: %w", t.dest(), err)
	}

	in, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}
	// The channel itself is the terminal's output, and with a pty that is all of
	// it: the far end's stderr goes to the terminal like everything else, which is
	// what makes a terminal one stream rather than two. (The library drains the
	// stderr stream anyway, in case a server sends something there regardless.)
	out, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}

	if line := s.line(); line == "" {
		err = sess.Shell() // no command: the login shell, as `ssh host` alone gives
	} else {
		err = sess.Start(line)
	}
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}
	return &remoteTerm{client: client, sess: sess, in: in, out: out}, nil
}

// Forward binds the local port and carries what arrives on it down the
// connection, which is what `ssh -L` is once there is no process to run it in.
//
// The two halves it is made of are the whole difference from the exec'd backend.
// One is a listener on this machine — the part that fails when the port is taken,
// and it fails here, immediately, rather than in the exit status of a process
// that had already started. The other is a direct-tcpip channel per accepted
// connection, opened only then: the server resolves "localhost" at its end, so
// what the tunnel reaches is what `-L port:localhost:port` has always reached.
//
// The connection belongs to the tunnel and goes with it, exactly as it does for a
// terminal: one tunnel is one connection, as one tunnel was one ssh process.
func (b goBackend) Forward(t Target, local, remote int) (Tunnel, error) {
	client, err := dial(t)
	if err != nil {
		return nil, err
	}
	lns, err := listenLoopback(local)
	if err != nil {
		client.Close()
		return nil, err
	}

	tun := &goTunnel{
		client: client,
		lns:    lns,
		// The far end as `-L local:localhost:remote` names it: a hostname for the
		// server to resolve, not an address resolved here. "localhost" on the server
		// is not "localhost" on this machine, and the difference is the point.
		remote: net.JoinHostPort("localhost", strconv.Itoa(remote)),
		done:   make(chan struct{}),
	}
	for _, ln := range lns {
		go tun.accept(ln)
	}
	go func() {
		// The connection ending is the tunnel ending, however it ends: a server
		// rebooted, a laptop's network gone, the keepalives giving up on a peer that
		// stopped answering.
		tun.finish(tun.client.Wait())
	}()
	return tun, nil
}

// listenLoopback binds a port on both loopback addresses, which is what OpenSSH
// does with a forward it was not told to bind anywhere in particular.
//
// Both, because "localhost" is two addresses on a modern machine and which one a
// program picks is not ours to predict: a browser opening localhost:3000 may well
// reach for ::1 first, and a tunnel that is only on 127.0.0.1 answers that with a
// refused connection or a stall.
//
// A machine with no IPv6 is the one case where one is enough, and it is the only
// one: a ::1 that exists and is *taken* is the port being in use, reported as
// such, because a tunnel that came up on half of localhost would be worse than
// one that said it could not — it would work from some programs and not others.
func listenLoopback(port int) ([]net.Listener, error) {
	addr := strconv.Itoa(port)
	first, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", addr))
	if err != nil {
		return nil, asPortBusy(port, err)
	}
	second, err := net.Listen("tcp", net.JoinHostPort("::1", addr))
	if err != nil {
		if busy := asPortBusy(port, err); errors.Is(busy, ErrPortBusy) {
			first.Close()
			return nil, busy
		}
		return []net.Listener{first}, nil // no IPv6 here at all
	}
	return []net.Listener{first, second}, nil
}

// asPortBusy names a bind failure that is another program holding the port, so
// the holder can say so instead of retrying it as a network blip.
//
// The check is the operating system's own errno rather than the message, since
// this backend never sees ssh's wording. Windows reports the same condition under
// a code of its own, where it falls through as an ordinary failure and the tunnel
// retries rather than reporting what is in the way — a worse message, not a worse
// tunnel, and only on Windows, which is the platform this client is least used
// on and the one where `FORGE_SSH_BACKEND=exec` is least likely to be an option.
func asPortBusy(port int, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%w: cannot listen to port: %d", ErrPortBusy, port)
	}
	return err
}

// goTunnel is a tunnel that is a listener and a connection, with no process
// anywhere in it.
type goTunnel struct {
	client *ssh.Client
	lns    []net.Listener
	remote string
	done   chan struct{}
	err    error
	once   sync.Once
	// closing is set before anything is torn down, so the connection ending is
	// read as the shutdown it is rather than as a tunnel that died.
	closing atomic.Bool
}

func (f *goTunnel) Wait() error {
	<-f.done
	return f.err
}

// Close takes the listeners down first and the connection after, so the local
// port is free before this returns — the holder may be about to rebind it.
func (f *goTunnel) Close() error {
	f.closing.Store(true)
	f.finish(nil)
	return nil
}

// finish settles the tunnel's answer, once, whichever of the two ends gets here
// first: the holder closing it, or the connection under it going away.
func (f *goTunnel) finish(err error) {
	f.once.Do(func() {
		for _, ln := range f.lns {
			ln.Close() // stops the accept loops, and releases the port
		}
		f.client.Close()
		if f.closing.Load() {
			err = nil
		}
		f.err = err
		close(f.done)
	})
}

// accept takes local connections and gives each one a channel of its own.
func (f *goTunnel) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // the listener was closed: the tunnel is over
		}
		go f.carry(conn)
	}
}

// carry joins one local connection to one channel on the far end.
//
// A far end that refuses is this connection's problem and nobody else's: the
// local caller gets what it would have got from a service that is not there, and
// the tunnel stays up. That is `-L` being lazy, and it is why a workspace whose
// container is down does not cost its tunnel anything.
func (f *goTunnel) carry(local net.Conn) {
	defer local.Close()
	far, err := f.client.Dial("tcp", f.remote)
	if err != nil {
		return
	}
	defer far.Close()

	// Both directions, and the first end to finish ends the pair: a peer that has
	// stopped reading is a connection neither side is using any more, and the
	// copies left running on it would be goroutines nothing will ever wake.
	ends := make(chan struct{}, 2)
	go func() { io.Copy(far, local); ends <- struct{}{} }()
	go func() { io.Copy(local, far); ends <- struct{}{} }()
	select {
	case <-ends:
	case <-f.done:
	}
}

// ptyModes are the terminal modes sent with the request.
//
// Echo on, and the line speeds every client sends because the request has fields
// for them. The rest is deliberately left to the server's own pty defaults: the
// exec'd client copies the modes off the local pty, and a pty Forge created a
// moment earlier has nothing configured on it that the remote one does not.
var ptyModes = ssh.TerminalModes{
	ssh.ECHO:          1,
	ssh.TTY_OP_ISPEED: 38400,
	ssh.TTY_OP_OSPEED: 38400,
}

// termType is the terminal type the far end is told about: this process's own,
// which is precisely what the exec'd ssh sends it.
//
// That includes sending nothing when there is nothing — a daemon started with no
// TERM has none to pass on, and ssh sends the empty string in the same
// situation. Giving every Forge terminal a TERM of its own is worth doing (the
// local shell already has one) but it is a change in what the server is told,
// and it belongs where all the terminals get it at once, not to one backend.
func termType() string { return os.Getenv("TERM") }

// remoteTerm is a Terminal whose pty is on the server. It owns the connection it
// was opened on, so closing it is what a terminal closing has always been: the
// connection goes, and with it the remote shell — or, for tmux, the client
// attached to a session that stays.
type remoteTerm struct {
	client *ssh.Client
	sess   *ssh.Session
	in     io.WriteCloser
	out    io.Reader
	once   sync.Once
}

func (r *remoteTerm) Read(p []byte) (int, error)  { return r.out.Read(p) }
func (r *remoteTerm) Write(p []byte) (int, error) { return r.in.Write(p) }

// Resize is the window-change request: the server resizes its pty and the
// program drawing into it gets a SIGWINCH.
func (r *remoteTerm) Resize(cols, rows uint16) error {
	return r.sess.WindowChange(int(rows), int(cols))
}

// Close ends the session and the connection under it. Idempotent, because the
// front end holding a terminal may well close it twice — a stream that ended and
// a panel that was replaced are two owners of the same object.
func (r *remoteTerm) Close() error {
	r.once.Do(func() {
		_ = r.sess.Close()
		_ = r.client.Close()
	})
	return nil
}

// dial opens a connection to the target and starts its keepalives.
//
// One connection per command, as the exec'd backend has always had: a Forge
// that holds clients open per host is the point of having a library client at
// all, but a cache has a lifetime, an eviction rule and a reconnect policy, and
// none of that can be reviewed against "the behaviour is unchanged". It comes
// with the step that measures it.
func dial(t Target) (*ssh.Client, error) {
	auth, err := authMethods()
	if err != nil {
		return nil, err
	}
	hostKeys, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	hops, err := ParseJump(t.Jump)
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}

	// The route, nearest hop first. Each is reached through the one before it, and
	// the target through the last — which is all `ssh -J` is: a jump host opens a
	// plain TCP stream to the next address and the handshake happens over it, so
	// the server at the end sees an ordinary connection and its key is checked
	// end to end. A jump host carries the bytes; it never reads them.
	var chain []*ssh.Client
	for _, hop := range hops {
		client, err := connect(last(chain), hop, auth, hostKeys)
		if err != nil {
			closeChain(chain)
			return nil, fmt.Errorf("ssh %s: via %s: %w", t.dest(), hop.via(), err)
		}
		// Keepalives on every hop, not only on the connection the caller holds. A
		// hop that stops answering without dying would otherwise be noticed by
		// nothing until something opened a channel on it — and opening that channel
		// is exactly what the next dial does.
		go keepalive(client)
		chain = append(chain, client)
	}

	client, err := connect(last(chain), t, auth, hostKeys)
	if err != nil {
		closeChain(chain)
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}
	if len(chain) > 0 {
		// The route has no life of its own: it exists to carry this connection, so
		// it goes when the connection does, however it goes. That keeps one
		// connection per command true — the caller closes what it was given and
		// nothing is left behind it.
		go func() {
			client.Wait()
			closeChain(chain)
		}()
	}
	go keepalive(client)
	return client, nil
}

// connect opens one connection: from this machine when through is nil, and
// otherwise over a stream the connection already established carries.
//
// Both halves end in the same handshake against the same host-key check, which
// is the point — a server reached through a jump is verified, and recorded on
// first sight, exactly as one reached directly.
func connect(through *ssh.Client, t Target, auth []ssh.AuthMethod, hostKeys ssh.HostKeyCallback) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: hostKeys,
		// The same bound the exec'd backend puts on ConnectTimeout, and for the
		// same reason: without one, an unreachable host is the operating
		// system's TCP timeout — over 45 seconds — and every command that
		// touches that host waits it out, the browser UI's workspace list
		// included.
		Timeout: connectTimeout * time.Second,
	}

	// The stream the handshake runs over: a socket from this machine, or one the
	// jump host opens on our behalf. From here the two are the same thing — which
	// is the whole reason a jumped connection behaves like a direct one.
	var (
		stream net.Conn
		err    error
	)
	if through == nil {
		stream, err = net.DialTimeout("tcp", t.addr(), cfg.Timeout)
	} else {
		// No ErrAuth reading on this one, unlike the handshake below: the jump has
		// already let us in by the time it is asked for a stream, so a refusal here
		// is its forwarding policy or a server that is not listening — something
		// that may well be different in a second, which is what a retry is for.
		stream, err = through.Dial("tcp", t.addr())
	}
	if err != nil {
		return nil, err
	}

	client, err := handshake(stream, t.addr(), cfg)
	if err != nil {
		// A refused key is named as such, here rather than at one call site, so it
		// means the same thing whichever shape of the transport ran into it. There
		// is nothing typed to match on: x/crypto's handshake failure is a formatted
		// string, so the wording is read once and turned into something that is not.
		if authFailed(err.Error()) {
			return nil, fmt.Errorf("%w: %w", ErrAuth, err)
		}
		return nil, err
	}
	return client, nil
}

// handshake turns a stream into a connection, and gives up on one that stops
// moving.
//
// The bound is enforced by hand because there is nowhere else to put it:
// ClientConfig.Timeout is applied by ssh.Dial to its own TCP connect and by
// NewClientConn to nothing at all, so a server that accepts a connection and
// then says nothing — a half-open firewall, a machine mid-reboot — leaves every
// command that touches it waiting for a version string that never comes. A
// deadline on the stream would do it for a socket, but not for the one a jump
// host hands back ("ssh: tcpChan: deadline not supported"); closing it ends the
// handshake either way, so that is what both get.
func handshake(stream net.Conn, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	type opened struct {
		conn  ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		err   error
	}
	done := make(chan opened, 1)
	go func() {
		conn, chans, reqs, err := ssh.NewClientConn(stream, addr, cfg)
		done <- opened{conn, chans, reqs, err}
	}()

	select {
	case h := <-done:
		if h.err != nil {
			stream.Close()
			return nil, h.err
		}
		return ssh.NewClient(h.conn, h.chans, h.reqs), nil
	case <-time.After(cfg.Timeout):
		// The goroutine is not left hanging: closing the stream fails the
		// handshake, which closes it in turn and sends to a buffered channel.
		stream.Close()
		return nil, fmt.Errorf("no answer within %s", cfg.Timeout)
	}
}

// last is the connection the next hop is opened over — nil for the first, which
// is dialled from this machine.
func last(chain []*ssh.Client) *ssh.Client {
	if len(chain) == 0 {
		return nil
	}
	return chain[len(chain)-1]
}

// closeChain takes a route down, furthest hop first.
func closeChain(chain []*ssh.Client) {
	for i := len(chain) - 1; i >= 0; i-- {
		chain[i].Close()
	}
}

// Keepalives, matching the exec'd backend's ServerAliveInterval=5 and
// ServerAliveCountMax=3. They are what notices a peer that dies *after* the
// connection is up — a laptop closed mid-checkpoint, a server rebooted under a
// long provisioning run — which no connect timeout covers. Without them the
// command hangs on a connection that will never answer again.
const (
	aliveInterval = 5 * time.Second
	aliveCountMax = 3
)

// keepalive pings the server until the connection is closed, giving up on it
// after aliveCountMax unanswered probes.
//
// A probe is counted as missed on a timeout as well as on an error, because the
// failure this guards against is precisely the connection that neither answers
// nor breaks: SendRequest on it blocks forever, so waiting for its error is
// waiting for the thing that never comes.
func keepalive(client *ssh.Client) {
	tick := time.NewTicker(aliveInterval)
	defer tick.Stop()

	gone := make(chan struct{})
	go func() {
		client.Wait() // returns when the connection ends, however it ends
		close(gone)
	}()

	misses := 0
	for {
		select {
		case <-gone:
			return
		case <-tick.C:
		}

		answered := make(chan bool, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err == nil
		}()
		select {
		case ok := <-answered:
			if ok {
				misses = 0
				continue
			}
			misses++
		case <-time.After(aliveInterval):
			misses++
		case <-gone:
			return
		}
		if misses >= aliveCountMax {
			client.Close()
			return
		}
	}
}

// authMethods is this device's key, and nothing else.
//
// One publickey method holding one signer. There is no agent to consult and no
// directory to walk: a server either trusts this device or it does not, which is
// the whole point of the device having a key of its own — see identity.go.
//
// Password and keyboard-interactive are absent rather than disabled, which is
// what the exec'd backend spends two options saying (PasswordAuthentication=no,
// KbdInteractiveAuthentication=no): a bad key must fail immediately and
// honestly instead of dropping into a prompt, which in the UI daemon is a
// prompt nobody is there to answer.
func authMethods() ([]ssh.AuthMethod, error) {
	signer, err := identity()
	if err != nil {
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}
