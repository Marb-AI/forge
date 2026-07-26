package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
// timeout, the same keepalives, the same key-only stance, and the same command
// string on the wire. Two gaps are known and deliberate, both closing with the
// device key in v2:
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

// authMethods offers the same identities the ssh binary would find: the agent
// first, then the default key files.
//
// Password and keyboard-interactive are absent rather than disabled, which is
// what the exec'd backend spends two options saying (PasswordAuthentication=no,
// KbdInteractiveAuthentication=no): a bad key must fail immediately and
// honestly instead of dropping into a prompt, which in the UI daemon is a
// prompt nobody is there to answer.
func authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// The agent, if one is running. It is first because it is where a key with a
	// passphrase — already unlocked, once, by whoever started the agent — lives.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	if signers := defaultIdentities(); len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable SSH key: no agent (SSH_AUTH_SOCK) and nothing readable in ~/.ssh (%s)",
			"id_ed25519, id_ecdsa, id_rsa")
	}
	return methods, nil
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
