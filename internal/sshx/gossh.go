package sshx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// goBackend is Forge's own SSH client: golang.org/x/crypto/ssh, no subprocess.
//
// It exists for the platforms where the exec'd backend cannot: a phone has no
// ssh binary and no way to spawn one, so either the client is library code or
// Forge is a laptop program forever. It is off by default (FORGE_SSH_BACKEND=go
// turns it on) because it is new against a client that has worked for years,
// and because two things it needs are not built yet — see below.
//
// Where it matches the exec'd backend it does so on purpose: the same connect
// timeout, the same keepalives, the same key-only stance, the same command string
// on the wire, and — for a terminal — the same terminal type and window size,
// asked of the server instead of taken from a pty on this machine. Two gaps are
// known and deliberate, both closing with the device key in v2:
//
//   - Credentials come from the agent and ~/.ssh, the same identities the ssh
//     binary would have found. Forge does not yet have a key of its own to
//     offer, and an encrypted key with no agent is skipped rather than
//     prompted for — this backend has no terminal to ask on.
//   - Host keys are read from ~/.ssh/known_hosts and never written. The exec'd
//     backend records a new host on first sight (StrictHostKeyChecking=
//     accept-new); doing that here means a trust store Forge owns, which is
//     its own step. Until then an unrecorded host is refused, and said so.
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

	// Before the pty request, and before the session starts: both of the agent's
	// halves are set up while the channel is still being configured. A failure is
	// not fatal, exactly as `ssh -A` without a running agent is not — the shell
	// opens, and git inside it asks for credentials it does not have.
	if s.ForwardAgent {
		forwardAgent(client, sess)
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

// forwardAgent lends the local agent to the session: a handler on this end for
// the channels the far end opens back, and the request that tells it forwarding
// is available.
//
// Both halves are best-effort for the same reason `ssh -A` is: the agent is a
// convenience for what runs *inside* the session, and a terminal that opens
// without it is far better than no terminal at all.
func forwardAgent(client *ssh.Client, sess *ssh.Session) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return
	}
	if err := agent.ForwardToRemote(client, sock); err != nil {
		return
	}
	_ = agent.RequestAgentForwarding(sess)
}

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
	auth, closeAuth, err := authMethods()
	if err != nil {
		return nil, err
	}
	// The agent is only consulted while the handshake runs, and ssh.Dial does not
	// return until that is over — so this is the moment its socket stops being
	// needed. Leaving it open would cost one file descriptor per command, which a
	// daemon polling every workspace notices within the day.
	defer closeAuth()
	hostKeys, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Addr, strconv.Itoa(port))

	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: hostKeys,
		// The same bound the exec'd backend puts on ConnectTimeout, and for the
		// same reason: without one, an unreachable host is the operating
		// system's TCP timeout — over 45 seconds — and every command that
		// touches that host waits it out, the browser UI's workspace list
		// included.
		Timeout: connectTimeout * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", t.dest(), err)
	}
	go keepalive(client)
	return client, nil
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

// authMethods offers the same identities the ssh binary would find: the agent's
// keys first, then the default key files. The returned func releases what they
// hold — today the agent's socket — and is safe to call more than once.
//
// They are offered as ONE publickey method, which is not a detail: x/crypto
// tries each authentication method by *name* and never returns to a name it has
// already tried, so an agent offered as its own method would mean a running
// agent with no useful key in it silently prevents ~/.ssh from ever being tried
// — a Forge that stops connecting because a colleague's agent is up. OpenSSH
// walks its identities inside the single publickey method; so does this.
//
// Password and keyboard-interactive are absent rather than disabled, which is
// what the exec'd backend spends two options saying (PasswordAuthentication=no,
// KbdInteractiveAuthentication=no): a bad key must fail immediately and
// honestly instead of dropping into a prompt, which in the UI daemon is a
// prompt nobody is there to answer.
func authMethods() ([]ssh.AuthMethod, func(), error) {
	var (
		signers []ssh.Signer
		open    []io.Closer
	)
	release := func() {
		for _, c := range open {
			c.Close()
		}
		open = nil
	}

	// The agent, if one is running. Its keys come first because that is where a
	// passphrase-protected key — already unlocked, once, by whoever started the
	// agent — lives. They stay usable only while the socket is open, since the
	// signing happens on the far side of it; that is why release runs after the
	// handshake rather than here.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			open = append(open, conn)
			if fromAgent, err := agent.NewClient(conn).Signers(); err == nil {
				signers = append(signers, fromAgent...)
			}
		}
	}

	signers = append(signers, defaultIdentities()...)
	if len(signers) == 0 {
		release()
		return nil, func() {}, fmt.Errorf("no usable SSH key: no agent (SSH_AUTH_SOCK) and nothing readable in ~/.ssh (%s)",
			"id_ed25519, id_ecdsa, id_rsa")
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, release, nil
}

// identityFiles are the key names OpenSSH tries by default, in its order.
var identityFiles = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// defaultIdentities loads whichever of them exist and can be parsed. A key that
// is missing, unreadable or passphrase-protected is skipped in silence: the
// server decides which of the offered keys it accepts, and one unusable file is
// not a reason to refuse to connect with the others.
func defaultIdentities() []ssh.Signer {
	dir, err := sshDir()
	if err != nil {
		return nil
	}
	var signers []ssh.Signer
	for _, name := range identityFiles {
		pem, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			continue
		}
		signers = append(signers, signer)
	}
	return signers
}

// hostKeyCallback verifies servers against ~/.ssh/known_hosts, and explains
// itself when the answer is "I have never seen this host".
//
// Read-only, and that is the difference from the exec'd backend worth knowing
// about: `ssh` would record an unknown host and carry on (accept-new), so a
// host prepared before this backend existed is trusted and a brand-new one is
// not. Writing the record needs a trust store Forge owns rather than one it
// borrows from OpenSSH, which is a step of its own.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	dir, err := sshDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "known_hosts")
	check, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s, so no server can be verified: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostname, remote, key)
		var unknown *knownhosts.KeyError
		if errors.As(err, &unknown) && len(unknown.Want) == 0 {
			return fmt.Errorf("host %s is not in %s: connect to it once with the default backend "+
				"(unset %s) to record its key", hostname, path, backendEnv)
		}
		return err
	}, nil
}

// sshDir is ~/.ssh, where this backend borrows its credentials from until the
// device key replaces them.
func sshDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate ~/.ssh: %w", err)
	}
	return filepath.Join(home, ".ssh"), nil
}
