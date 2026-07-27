package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Which servers this device trusts, and who writes that down.
//
// The exec'd backend answers this with an ssh option: StrictHostKeyChecking=
// accept-new records a server the first time it is seen and refuses it loudly
// if its key ever changes afterwards, in ~/.ssh/known_hosts. That is the right
// policy for what Forge is — you own the servers it connects to, and there is
// nobody at the terminal to confirm a fingerprint — but the file it is written
// in belongs to OpenSSH, and the client that has to work on a phone cannot ask
// OpenSSH for anything. Until now Forge's own client could only *read* that
// file, so a host prepared before it existed was trusted and a new one was
// refused; this is the other half.
//
// So the same policy, in a file of Forge's own, next to the rest of this
// device's state:
//
//   - A server nobody has recorded is trusted on sight and written down. This
//     is the moment a man in the middle would have to be there already, which
//     is the whole of what trust-on-first-use claims and no more.
//   - A server whose key does not match what was written down is refused, and
//     the error names the file and line holding the old key. Nothing rewrites
//     that line: a key that changed is either a rebuilt server or an attack,
//     and only the person reading the message knows which.
//   - ~/.ssh/known_hosts is still read, and never written. A host the ssh
//     binary already trusts is not a host to accept on sight a second time,
//     and both clients agreeing on which servers are known is the point of
//     being able to switch between them. It is read-only because it is not
//     ours: the write is the part that has to move, and it has.
//
// The last of those goes when the device key does, in v2 — that is the release
// where Forge stops borrowing anything from ~/.ssh at all.
//
// A file, rather than a Store interface like the config and the device key.
// Those two are abstract because their answer genuinely differs per platform: a
// private key belongs in the Keychain on iOS and may never leave the chip at
// all. Host keys are public, small and the same everywhere, the state directory
// is a real directory on every platform Forge targets, and the library's
// matcher — hashed hostnames, ports, wildcards, revocation — only reads files.
// An interface here would mean reimplementing that matching for no platform
// that needs it.

// knownHostsFile is the name of the file inside the state directory. OpenSSH's
// name for it, because it is OpenSSH's format: a line of it can be pasted into
// ~/.ssh/known_hosts and vice versa.
const knownHostsFile = "known_hosts"

// stateDir is how the transport finds that directory: a function, given by
// whoever resolved this device's state, called at the moment a connection needs
// it. A function rather than a path so that asking does not create a directory
// on a Forge that never connects to anything.
//
// Process-wide and set at startup, like the chosen backend and the core's own
// stores — see Use.
var (
	stateDirMu sync.Mutex
	stateDir   func() (string, error)
)

// errNoStateDir is nothing having said where this device keeps its state. It is
// a wiring mistake rather than a user's problem: the core points the transport
// at its own directory when it resolves it, and every operation that reaches a
// server has asked the core for the host's address first.
var errNoStateDir = errors.New("nothing told the transport where this device keeps its state")

// KnownHostsIn tells the transport where to keep the servers it has accepted.
// The core calls it with its own state directory.
func KnownHostsIn(dir func() (string, error)) {
	stateDirMu.Lock()
	defer stateDirMu.Unlock()
	stateDir = dir
}

// KnownHosts is the file those servers are recorded in. It creates nothing —
// the answer is a path, whether or not anything has been written there yet.
func KnownHosts() (string, error) {
	stateDirMu.Lock()
	dir := stateDir
	stateDirMu.Unlock()
	if dir == nil {
		return "", errNoStateDir
	}
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, knownHostsFile), nil
}

// recordMu serialises the read-and-append below. Two connections to the same
// unknown host start at the same moment often enough to matter: the UI daemon
// polls every workspace on a host at once, and every one of those is a first
// sight of it.
var recordMu sync.Mutex

// hostKeyCallback verifies servers against what this device has recorded, and
// records the ones it has never seen.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	ours, ourErr := KnownHosts()
	var files []string
	if ourErr == nil {
		// knownhosts.New refuses a file that is not there, and a device that has
		// trusted nothing yet has no file. Empty is the honest starting point: it
		// says "nothing is known", where a missing file says "nothing can be
		// checked".
		if err := touch(ours); err != nil {
			return nil, fmt.Errorf("cannot record host keys in %s: %w", ours, err)
		}
		files = append(files, ours)
	}
	if legacy, ok := opensshKnownHosts(); ok {
		files = append(files, legacy)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no host keys to check servers against: %w", ourErr)
	}

	check, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s, so no server can be verified: %w",
			strings.Join(files, " or "), err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostname, remote, key)
		var unknown *knownhosts.KeyError
		if !errors.As(err, &unknown) {
			// Verified, or refused for a reason of its own — a revoked key, which
			// is knownhosts' other error and means what it says.
			return err
		}
		if len(unknown.Want) > 0 {
			return changedKey(hostname, key, unknown)
		}
		if ours == "" {
			return fmt.Errorf("host %s has never been seen and there is nowhere to record it: %w",
				hostname, ourErr)
		}
		if err := record(ours, hostname, key); err != nil {
			return fmt.Errorf("host %s cannot be recorded in %s: %w", hostname, ours, err)
		}
		return nil
	}, nil
}

// changedKey is the one failure here worth spelling out. The library's own
// wording ("ssh: handshake failed: knownhosts: key mismatch") names neither the
// host, nor the key, nor the line that has to go for the connection to work
// again — and the person reading it is the only one who can tell a rebuilt
// server from an intercepted one.
func changedKey(hostname string, offered ssh.PublicKey, e *knownhosts.KeyError) error {
	var recorded []string
	for _, k := range e.Want {
		recorded = append(recorded, fmt.Sprintf("%s (%s line %d)",
			ssh.FingerprintSHA256(k.Key), k.Filename, k.Line))
	}
	return fmt.Errorf("the host key for %s has CHANGED: it offers %s, this device trusts %s. "+
		"If you rebuilt the server, delete that line and connect again; if you did not, "+
		"something is answering in its place",
		hostname, ssh.FingerprintSHA256(offered), strings.Join(recorded, ", "))
}

// record writes one host down, and says so.
//
// The line is re-read before it is appended rather than trusted from the check
// a moment ago: under recordMu that closes the window between two connections
// to the same new host, and it also means a file somebody has edited by hand in
// the meantime does not collect a duplicate.
func record(path, hostname string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)

	recordMu.Lock()
	defer recordMu.Unlock()

	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, have := range strings.Split(string(current), "\n") {
		if strings.TrimSpace(have) == line {
			return nil
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// A file whose last line has no newline would otherwise swallow this one.
	if len(current) > 0 && current[len(current)-1] != '\n' {
		line = "\n" + line
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// On stderr, where `ssh` puts the same sentence ("Warning: Permanently added
	// …"). It is the one thing this policy does silently that the user might
	// want to have seen, and Output leaves stderr attached for exactly this
	// class of message; under the UI daemon it lands in its log.
	fmt.Fprintf(os.Stderr, "forge: trusting %s on first connection (%s), recorded in %s\n",
		hostname, ssh.FingerprintSHA256(key), path)
	return nil
}

// touch makes sure the file exists, without changing one that already does.
func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// opensshKnownHosts is ~/.ssh/known_hosts if this machine has one — the second
// opinion, read and never written. A machine without one (a phone, a container)
// simply has no second opinion.
func opensshKnownHosts() (string, bool) {
	dir, err := sshDir()
	if err != nil {
		return "", false
	}
	path := filepath.Join(dir, knownHostsFile)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}
